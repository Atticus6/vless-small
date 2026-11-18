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
	"syscall"
	"time"
)

// version 由 Release workflow 通过 -ldflags "-X main.version=<tag>" 注入；
// 本地 go run/go build 时为 dev。
var version = "dev"

func main() {
	cfg := parseFlags()

	// 版本查询必须在配置校验之前：安装脚本用它确认二进制可执行。
	if cfg.showVersion {
		fmt.Printf("vless-small %s\n", version)
		return
	}

	nodeCfg, err := parseNodeConfig(cfg.controlPlane)
	if err != nil {
		log.Fatalf("invalid CONFIG: %v", err)
	}

	users, err := parseUsers(cfg.uuidList)
	if err != nil {
		log.Fatalf("invalid users: %v", err)
	}
	if len(users) == 0 && nodeCfg.baseURL == "" {
		log.Fatal("missing -uuid (or UUID env) or -config (or CONFIG env)")
	}

	srv := newServer(users, cfg.wsPath)
	srv.controlToken = nodeCfg.token
	tunnelCtl := newTunnelController(cfg.listen)
	defer tunnelCtl.Close()
	startNodeSync(cfg, nodeCfg, srv, users, tunnelCtl)

	httpServer := &http.Server{
		Addr:              cfg.listen,
		Handler:           http.HandlerFunc(srv.handleHTTP),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Ctrl+C / SIGTERM 时优雅退出.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		// Shutdown 不跟踪 hijack 后的 WebSocket, 再给转发会话一段排空时间.
		srv.waitDrain(5 * time.Second)
	}()

	log.Printf(
		"vless-ws listening on %s (path=%s, users=%d)",
		cfg.listen, displayPath(cfg.wsPath), srv.userCount(),
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

// startNodeSync 在配置了 CONFIG 时执行首次上报，并按平台决定是否后台轮询.
func startNodeSync(
	cfg config,
	nodeCfg nodeConfig,
	srv *server,
	users map[[16]byte]struct{},
	tunnelCtl tunnelController,
) {
	if nodeCfg.baseURL == "" {
		return
	}

	info := collectNodeInfo()
	log.Printf(
		"node info runtime=%s hosts=%v ipv4=%s ipv6=%s country=%s tunnel=%v",
		info.runtime, info.hosts, info.ipv4, info.ipv6, info.countryCode,
		tunnelSupported(),
	)

	tsnetCtl := newTSNetController(nodeCfg, tsnetHandler(srv))
	result, err := syncDevices(
		nodeCfg, srv, cfg.listen, cfg.wsPath, users, info,
		tsnetCtl.IP(), tunnelCtl.Status(),
	)
	if err != nil {
		log.Printf("initial device sync failed: %v", err)
	}
	if result != nil {
		tsnetCtl.Sync(result.Tsnet != nil && result.Tsnet.Enabled)
		if result.Tunnel != nil {
			tunnelCtl.Sync(result.Tunnel.Enabled)
		}
		handleNodeUpdate(nodeCfg, result.Update)
	}

	// Vercel / Railway 无请求时会自动休眠；后台定时同步会产生持续请求，
	// 让实例无法休眠。这两个平台只在冷启动时同步一次。
	if isAutoSleepRuntime(info.runtime) {
		log.Printf("runtime %s: background sync disabled (auto-sleep)", info.runtime)
		return
	}
	go syncLoop(nodeCfg, srv, cfg.listen, cfg.wsPath, users, info, tsnetCtl, tunnelCtl)
}

func displayPath(path string) string {
	if path == "" {
		return "*"
	}
	return path
}
