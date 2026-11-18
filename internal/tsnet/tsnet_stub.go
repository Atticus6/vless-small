//go:build vercel

package tsnet

import (
	"log"
	"net/http"
	"sync"
)

// Vercel 构建使用 `-tags vercel`，这里提供与 tsnet_impl.go 相同的入口，
// 不导入 tailscale.com，确保打包产物完全剔除 tsnet 代码。
func Supported() bool { return false }

func New(_ Config, _ http.Handler) Controller {
	return &ControllerStub{}
}

type ControllerStub struct {
	mu      sync.Mutex
	logged  bool
	enabled bool
}

func (c *ControllerStub) Sync(enabled bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.enabled = enabled
	if enabled && !c.logged {
		c.logged = true
		log.Printf("tsnet: 当前为 vercel 构建，未包含 tsnet 支持")
	}
}

func (c *ControllerStub) IP() string { return "" }

func (c *ControllerStub) Close() {
	c.Sync(false)
}
