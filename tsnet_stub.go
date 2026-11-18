//go:build vercel

package main

import (
	"log"
	"net/http"
	"sync"
)

// Vercel 构建使用 `-tags vercel`，这里提供与 tsnet_impl.go 相同的入口，
// 不导入 tailscale.com，确保打包产物完全剔除 tsnet 代码。
func tsnetSupported() bool { return false }

func newTSNetController(_ nodeConfig, _ http.Handler) tsnetController {
	return &tsnetControllerStub{}
}

type tsnetControllerStub struct {
	mu      sync.Mutex
	logged  bool
	enabled bool
}

func (c *tsnetControllerStub) Sync(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.enabled = enabled
	if enabled && !c.logged {
		c.logged = true
		log.Printf("tsnet: 当前为 vercel 构建，未包含 tsnet 支持")
	}
}

func (c *tsnetControllerStub) IP() string { return "" }

func (c *tsnetControllerStub) Close() {
	c.Sync(false)
}
