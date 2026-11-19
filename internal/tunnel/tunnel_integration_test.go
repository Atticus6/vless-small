//go:build !vercel

package tunnel

import (
	"fmt"
	"io"
	"net"
	"net/http"
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

// TestQuickTunnelProvisioning 是可选的真实临时隧道集成测试。
//
// 它会向 Cloudflare 申请一条无账号 quick tunnel，验证固定隧道未分配时
// 的兜底路径。默认跳过；运行：
//
//	TUNNEL_QUICK_INTEGRATION=1 go test ./internal/tunnel -run TestQuickTunnelProvisioning -v
func TestQuickTunnelProvisioning(t *testing.T) {
	if os.Getenv("TUNNEL_QUICK_INTEGRATION") != "1" {
		t.Skip("set TUNNEL_QUICK_INTEGRATION=1 to run")
	}

	// 本地回源服务：临时隧道地址最终要能访问到它。
	listener, err := net.Listen("tcp", ":18081")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "ok")
		}),
	}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	ctl := New(":18081")
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true})
	url := waitTunnelURL(t, ctl, 45*time.Second)
	if !strings.HasPrefix(url, "https://") ||
		!strings.HasSuffix(url, ".trycloudflare.com") {
		t.Fatalf("URL = %q, want a trycloudflare.com address", url)
	}
	t.Logf("quick tunnel: %s", url)

	// 边缘建连和 DNS 生效可能需要几秒，轮询直到回源成功。
	client := &http.Client{Timeout: 10 * time.Second}
	deadline := time.Now().Add(40 * time.Second)
	var lastErr error
	reachable := false
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 16))
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK &&
				string(body) == "ok" {
				reachable = true
				break
			}
			lastErr = fmt.Errorf(
				"status=%d body=%q readErr=%v",
				resp.StatusCode, body, readErr,
			)
		} else {
			lastErr = err
		}
		time.Sleep(2 * time.Second)
	}
	if !reachable {
		t.Fatalf("quick tunnel not reachable: %v", lastErr)
	}

	ctl.Sync(Signal{})
	if status := ctl.Status(); status.URL != "" {
		t.Fatalf("URL not cleared after stop: %q", status.URL)
	}
}
