//go:build !vercel

package tunnel

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestQuickTunnelProvisioning 是可选的真实网络集成测试.
//
// 默认跳过；需要验证 Cloudflare Quick Tunnel 分配、启动/停止和重复启动时运行：
//
//	TUNNEL_INTEGRATION=1 go test -run TestQuickTunnelProvisioning -v .
//
// 测试会向 api.trycloudflare.com 申请临时隧道，可能分配公网地址。
func TestQuickTunnelProvisioning(t *testing.T) {
	if os.Getenv("TUNNEL_INTEGRATION") != "1" {
		t.Skip("set TUNNEL_INTEGRATION=1 to run")
	}

	ctl := New(":18080")
	defer ctl.Close()

	for round := 1; round <= 2; round++ {
		ctl.Sync(true)
		url := waitTunnelURL(t, ctl, 30*time.Second)
		if !strings.HasPrefix(url, "https://") || !strings.Contains(url, ".trycloudflare.com") {
			t.Fatalf("round %d: unexpected tunnel URL: %q", round, url)
		}
		t.Logf("round %d: %s", round, url)

		ctl.Sync(false)
		if status := ctl.Status(); status.URL != "" {
			t.Fatalf("round %d: URL not cleared after stop: %q", round, status.URL)
		}
	}
}

func waitTunnelURL(t *testing.T, ctl Controller, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status := ctl.Status()
		if status.URL != "" {
			return status.URL
		}
		if status.Error != "" {
			t.Fatalf("tunnel error: %s", status.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("timeout waiting for tunnel URL")
	return ""
}
