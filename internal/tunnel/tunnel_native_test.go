//go:build !cloudflared

package tunnel

import (
	"errors"
	"testing"
)

// TestNativeNewLocalURL 验证回源地址跟随实际监听地址：
// 通配地址回源到 127.0.0.1，显式绑定内网地址时回源到同一地址。
func TestNativeNewLocalURL(t *testing.T) {
	tests := []struct {
		listen string
		want   string
	}{
		{listen: ":8080", want: "http://127.0.0.1:8080"},
		{listen: "192.168.1.10:9000", want: "http://192.168.1.10:9000"},
		{listen: "[::1]:8081", want: "http://[::1]:8081"},
	}

	for _, tt := range tests {
		ctl := New(tt.listen).(*quickTunnel)
		if ctl.localURL != tt.want {
			t.Fatalf("New(%q).localURL = %q, want %q", tt.listen, ctl.localURL, tt.want)
		}
		ctl.Close()
	}
}

// TestSetURL 覆盖异步申请成功后的状态写入，以及旧 generation 的回调隔离。
func TestSetURL(t *testing.T) {
	tun := &quickTunnel{generation: 2}

	tun.setURL(1, "https://old.trycloudflare.com")
	if status := tun.Status(); status.URL != "" {
		t.Fatalf("stale generation updated URL: %q", status.URL)
	}

	tun.setURL(2, "https://new.trycloudflare.com")
	status := tun.Status()
	if status.URL != "https://new.trycloudflare.com" || status.Error != "" {
		t.Fatalf("status = %+v", status)
	}
	if !status.Supported {
		t.Fatal("tunnel should report supported")
	}
}

// TestSetURLAfterClose 验证关闭后迟到的回调不会复活状态。
func TestSetURLAfterClose(t *testing.T) {
	tun := &quickTunnel{generation: 1, closed: true}

	tun.setURL(1, "https://late.trycloudflare.com")
	if status := tun.Status(); status.URL != "" {
		t.Fatalf("closed tunnel got URL: %q", status.URL)
	}
}

// TestRunExitedClearsCancel 验证 run 退出后清空 URL、记录错误并允许重试。
func TestRunExitedClearsCancel(t *testing.T) {
	tun := &quickTunnel{
		generation: 3,
		enabled:    true,
		cancel:     func() {},
		url:        "https://abc.trycloudflare.com",
	}

	tun.runExited(3, errors.New("edge unreachable"))

	status := tun.Status()
	// Run 重启会重新申请凭证并得到新地址，旧 URL 必须清空。
	if status.URL != "" {
		t.Fatalf("URL = %q, want cleared", status.URL)
	}
	if status.Error != "edge unreachable" {
		t.Fatalf("error = %q", status.Error)
	}
	if tun.cancel != nil {
		t.Fatal("cancel 未清空，下一次 Sync 不会重试")
	}
}

// TestRunExitedIgnoresStaleGeneration 验证旧 run 的错误不会覆盖新状态。
func TestRunExitedIgnoresStaleGeneration(t *testing.T) {
	tun := &quickTunnel{
		generation: 5,
		cancel:     func() {},
		url:        "https://new.trycloudflare.com",
	}

	tun.runExited(4, errors.New("old error"))

	status := tun.Status()
	if status.URL != "https://new.trycloudflare.com" || status.Error != "" {
		t.Fatalf("status = %+v", status)
	}
	if tun.cancel == nil {
		t.Fatal("旧 run 清掉了当前代的 cancel")
	}
}

// TestSyncDisabledClearsState 验证关闭开关会取消并清空 URL/错误。
func TestSyncDisabledClearsState(t *testing.T) {
	canceled := false
	tun := &quickTunnel{
		enabled: true,
		cancel:  func() { canceled = true },
		url:     "https://abc.trycloudflare.com",
		lastErr: "boom",
	}

	tun.Sync(Signal{})

	if !canceled {
		t.Fatal("cancel 未调用")
	}
	status := tun.Status()
	if status.URL != "" || status.Error != "" {
		t.Fatalf("status = %+v", status)
	}
	if tun.enabled {
		t.Fatal("enabled 仍为 true")
	}
}

// TestSyncNoopWhenDisabled 验证关闭状态重复 Sync 不会启动新隧道。
func TestSyncNoopWhenDisabled(t *testing.T) {
	tun := &quickTunnel{}
	tun.Sync(Signal{})
	if tun.cancel != nil {
		t.Fatal("disabled Sync started a tunnel")
	}
	if tun.generation != 0 {
		t.Fatalf("generation = %d", tun.generation)
	}
}

// TestSyncEnabledInvalidListen 验证监听地址非法时只记录错误，不发网络请求。
func TestSyncEnabledInvalidListen(t *testing.T) {
	ctl := New("nonsense")
	defer ctl.Close()

	ctl.Sync(Signal{Enabled: true})
	status := ctl.Status()
	if status.URL != "" {
		t.Fatalf("URL = %q, want empty", status.URL)
	}
	if status.Error == "" {
		t.Fatal("expected listen error in status")
	}
}
