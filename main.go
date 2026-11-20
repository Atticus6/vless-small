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
	"syscall"
	"time"

	"github.com/atticus6/vless-small/internal/app"
	"github.com/atticus6/vless-small/internal/cert"
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

func displayPath(path string) string {
	if path == "" {
		return "*"
	}
	return path
}
