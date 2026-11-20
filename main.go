// Command vless-small 是最小可用的 VLESS over WebSocket 服务端.
//
// 只做三件事: WebSocket 握手、VLESS 头解析与 UUID 认证、TCP/UDP 双向转发.
// 不包含 TLS、隧道、管理后台; 需要加密时把本服务放在 Nginx/Cloudflare 之后.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/atticus6/vless-small/internal/app"
	"github.com/atticus6/vless-small/internal/cert"
	"github.com/atticus6/vless-small/internal/hy2"
	"github.com/atticus6/vless-small/internal/platform"
	"github.com/atticus6/vless-small/internal/tunnel"
)

// version 由 Release workflow 通过 -ldflags "-X main.version=<tag>" 注入；
// 本地 go run/go build 时为 dev。ldflags 只能设置 main 包变量，因此启动时
// 再复制给 app.Version 供上报和 /healthz 使用。
var version = "dev"

// tuneGC 在用户没有显式设置 GOGC 时放宽 GC 目标。
//
// 转发路径已经用池化缓冲把稳态分配压得很低，但连接建立/断开仍会产生一批
// 短生命周期对象；默认 GOGC=100 会让这些对象触发的 GC 更频繁地抢占转发
// goroutine 的 CPU。放宽到 200 用更多堆内存换更少的 GC 周期，对延迟和吞吐
// 都更友好。内存受限的节点仍可用 GOGC=50/100 或 GOMEMLIMIT 覆盖；只要显式
// 设置了 GOGC，这里就不改动运行时的默认值。
func tuneGC() {
	if os.Getenv("GOGC") != "" {
		return
	}
	debug.SetGCPercent(200)
}

func main() {
	tuneGC()

	cfg := parseFlags()
	app.Version = version
	// 纯 Go Quick Tunnel 实现用版本号构造 User-Agent 和注册信息；
	// cloudflared 构建忽略这个值，两种实现共用同一注入入口。
	tunnel.Version = version

	// 版本查询必须在配置校验之前：安装脚本用它确认二进制可执行。
	if cfg.showVersion {
		fmt.Printf("vless-small %s\n", version)
		return
	}

	// Linux 纯本机环境尽力开启 BBR；容器/PaaS/无权限时只记日志，不阻塞启动。
	platform.TryEnableBBR()

	nodeCfg, err := app.ParseNodeConfig(cfg.controlPlane)
	if err != nil {
		log.Fatalf("invalid CONFIG: %v", err)
	}

	users, err := app.ParseUsers(cfg.uuidList)
	if err != nil {
		log.Fatalf("invalid users: %v", err)
	}
	if len(users) == 0 && nodeCfg.BaseURL == "" {
		log.Fatal("missing -uuid (or UUID env) or -config (or CONFIG env)")
	}

	// 配置确认后才尝试系统级调优；Linux 纯本机环境尽力开启 BBR，
	// 容器/PaaS/无权限时只记日志，不阻塞启动。
	platform.TryEnableBBR()

	srv := app.NewServer(users, cfg.wsPath)
	srv.SetControlToken(nodeCfg.Token)

	handler := http.HandlerFunc(srv.HandleHTTP)
	tunnelCtl := tunnel.New(cfg.listen)
	certCtl := cert.New(handler)
	srv.SetStatusProviders(tunnelCtl.Status, certCtl.Status)
	defer tunnelCtl.Close()
	defer certCtl.Close()

	// 可选的 Hysteria 2 入口：UDP/QUIC 直连节点，不经过 CDN。
	// 只有显式设置 HY2_LISTEN 才启动；配置错误直接退出，避免带着坏配置跑。
	var hy2Srv *hy2.Server
	if cfg.hy2Listen != "" {
		hy2Srv, err = startHysteria2(cfg, nodeCfg)
		if err != nil {
			log.Fatalf("hysteria2: %v", err)
		}
		if hy2Srv != nil {
			defer hy2Srv.Close()
			go func() {
				log.Printf("hysteria2 listening on %s", hy2Srv.Addr())
				if err := hy2Srv.Serve(); err != nil {
					log.Printf("hysteria2: %v", err)
				}
			}()
		}
	}

	app.StartNodeSync(cfg.listen, cfg.wsPath, nodeCfg, srv, users, tunnelCtl, certCtl)

	httpServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Ctrl+C / SIGTERM 时优雅退出.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		if hy2Srv != nil {
			// 先停止接受新的 QUIC 连接，再排空 WS 会话。
			hy2Srv.Close()
		}
		// 先关证书的 80/443 监听：它由 certCtl 独立持有，httpServer.Shutdown
		// 管不到。不先关的话，排空期间仍能建立新的 HTTPS WebSocket 会话，
		// 退出前永远等不到 active 归零。
		certCtl.Close()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		// Shutdown 不跟踪 hijack 后的 WebSocket, 再给转发会话一段排空时间.
		srv.WaitDrain(5 * time.Second)
		// 尽量把最后一个 30 分钟窗口内的流量发出去。
		app.FlushTraffic(nodeCfg, srv)
	}()

	log.Printf(
		"vless-ws listening on %s (path=%s, users=%d)",
		cfg.listen, displayPath(cfg.wsPath), srv.UserCount(),
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

// startHysteria2 探测端口可达性后启动 Hysteria 2 入口。
//
// Hysteria 走 UDP，TCP 连通性测试对它没有意义；这里用 STUN 拿公网映射
// 地址，只有映射端口与监听端口一致（说明没有 NAT 改写、端口确实对外开放）
// 才启动。否则直接跳过并返回 nil：客户端会连上一个永远打不开的入口，
// 不如让节点只提供 WS 入口。
//
// 显式设置 HY2_SKIP_PROBE=1 可以跳过探测强制启动，适用于探测被防火墙
// 干扰但实际可达的环境。
func startHysteria2(cfg config, nodeCfg app.NodeConfig) (*hy2.Server, error) {
	if !skipHy2Probe() {
		info := app.PublicIPv4()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		result := hy2.ProbeReachability(ctx, cfg.hy2Listen, info)
		cancel()

		if !result.Reachable {
			log.Printf("hysteria2: skip, %s", result.Reason)
			return nil, nil
		}
		log.Printf("hysteria2: port reachable, %s", result.Reason)
	}

	srv, err := hy2.New(hy2.Config{
		Listen:           cfg.hy2Listen,
		Password:         cfg.hy2Password,
		CertFile:         cfg.hy2Cert,
		KeyFile:          cfg.hy2Key,
		SelfSignedDomain: cfg.hy2Domain,
		Dial:             app.DialOutbound,
		Logf:             log.Printf,
	})
	if err != nil {
		return nil, err
	}
	return srv, nil
}

// skipHy2Probe 判断是否跳过端口可达性探测。
func skipHy2Probe() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HY2_SKIP_PROBE"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

func displayPath(path string) string {
	if path == "" {
		return "*"
	}
	return path
}
