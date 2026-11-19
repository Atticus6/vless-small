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

	"github.com/atticus6/vless-small/internal/app"
	"github.com/atticus6/vless-small/internal/cert"
	"github.com/atticus6/vless-small/internal/tunnel"
)

// version 由 Release workflow 通过 -ldflags "-X main.version=<tag>" 注入；
// 本地 go run/go build 时为 dev。ldflags 只能设置 main 包变量，因此启动时
// 再复制给 app.Version 供上报和 /healthz 使用。
var version = "dev"

func main() {
	cfg := parseFlags()
	app.Version = version
	tunnel.Version = version

	// 版本查询必须在配置校验之前：安装脚本用它确认二进制可执行。
	if cfg.showVersion {
		fmt.Printf("vless-small %s\n", version)
		return
	}

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
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		// Shutdown 不跟踪 hijack 后的 WebSocket, 再给转发会话一段排空时间.
		srv.WaitDrain(5 * time.Second)
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
