//go:build !vercel

package tunnel

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestFixedTunnelProvisioning 是可选的真实固定隧道集成测试。
//
// 默认跳过；准备一个已创建、DNS 已指向 {tunnelId}.cfargotunnel.com 的
// Cloudflare 命名隧道，然后运行：
//
//	TUNNEL_INTEGRATION=1 \
//	TUNNEL_TOKEN='<cloudflare tunnel token>' \
//	TUNNEL_HOSTNAME='hk-tunnel.atticus.eu.org' \
//	go test ./internal/tunnel -run TestFixedTunnelProvisioning -v
func TestFixedTunnelProvisioning(t *testing.T) {
	if os.Getenv("TUNNEL_INTEGRATION") != "1" {
		t.Skip("set TUNNEL_INTEGRATION=1 to run")
	}

	token := strings.TrimSpace(os.Getenv("TUNNEL_TOKEN"))
	hostname := normalizeTunnelHostname(os.Getenv("TUNNEL_HOSTNAME"))
	if token == "" || hostname == "" {
		t.Skip("TUNNEL_TOKEN and TUNNEL_HOSTNAME are required")
	}

	ctl := New(":18080")
	defer ctl.Close()

	for round := 1; round <= 2; round++ {
		ctl.Sync(Signal{Enabled: true, Token: token, Hostname: hostname})
		url := waitTunnelURL(t, ctl, 30*time.Second)
		if url != "https://"+hostname {
			t.Fatalf("round %d: URL = %q, want https://%s", round, url, hostname)
		}
		t.Logf("round %d: %s", round, url)

		ctl.Sync(Signal{})
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
