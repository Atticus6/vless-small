//go:build !vercel

package tunnel

import (
	"errors"
	"testing"
)

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

// TestRunExitedClearsCancel 验证 run 退出后保留 URL、记录错误并允许重试。
func TestRunExitedClearsCancel(t *testing.T) {
	tun := &quickTunnel{
		generation: 3,
		enabled:    true,
		cancel:     func() {},
		url:        "https://abc.trycloudflare.com",
	}

	tun.runExited(3, errors.New("edge unreachable"))

	status := tun.Status()
	if status.URL != "https://abc.trycloudflare.com" {
		t.Fatalf("URL = %q, want kept", status.URL)
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

	tun.Sync(false)

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
	tun.Sync(false)
	if tun.cancel != nil {
		t.Fatal("disabled Sync started a tunnel")
	}
	if tun.generation != 0 {
		t.Fatalf("generation = %d", tun.generation)
	}
}
