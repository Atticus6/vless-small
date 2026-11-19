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
// 它会向 Cloudflare 申请一条无账号 quick tunnel，验证临时隧道从申请到
// 回源的完整路径。默认跳过；运行：
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
