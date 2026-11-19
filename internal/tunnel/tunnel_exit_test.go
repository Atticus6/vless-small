//go:build !vercel

package tunnel

import (
	"errors"
	"testing"
)

// TestSupervisorExitClearsState 验证 supervisor 意外退出后会清空 URL 与
// cancel，使下一次 Sync 能重新启动隧道（而不是永久保留失效状态）。
func TestSupervisorExitClearsState(t *testing.T) {
	canceled := false
	tun := &controller{
		enabled:    true,
		cancel:     func() { canceled = true },
		url:        "https://hk-tunnel.atticus.eu.org",
		generation: 3,
	}

	tun.supervisorExited(3, errors.New("connection refused"))

	status := tun.Status()
	if status.URL != "" {
		t.Fatalf("URL = %q, want empty", status.URL)
	}
	if status.Error != "connection refused" {
		t.Fatalf("error = %q, want the supervisor error", status.Error)
	}
	if !canceled {
		t.Fatal("cancel 未调用，旧上下文泄漏")
	}
	if tun.cancel != nil {
		t.Fatal("cancel 未清空，Sync 不会重试")
	}
}

// TestSupervisorExitWithoutErrorUsesRetryMessage 覆盖 ctx 未取消但 supervisor
// 正常返回的情况：仍然要清空状态并给出可重试的提示。
func TestSupervisorExitWithoutErrorUsesRetryMessage(t *testing.T) {
	tun := &controller{
		enabled:    true,
		url:        "https://old-tunnel.atticus.eu.org",
		generation: 1,
	}

	tun.supervisorExited(1, nil)

	status := tun.Status()
	if status.URL != "" {
		t.Fatalf("URL = %q, want empty", status.URL)
	}
	if status.Error == "" {
		t.Fatal("error 为空，用户无法知道隧道已断开")
	}
}

// TestSupervisorExitIgnoresStaleGeneration 验证旧代 supervisor 的退出事件
// 不会清掉新一代已经建立的隧道。
func TestSupervisorExitIgnoresStaleGeneration(t *testing.T) {
	tun := &controller{
		enabled:    true,
		cancel:     func() {},
		url:        "https://new-tunnel.atticus.eu.org",
		generation: 5,
	}

	tun.supervisorExited(4, errors.New("old tunnel failed"))

	status := tun.Status()
	if status.URL != "https://new-tunnel.atticus.eu.org" {
		t.Fatalf("URL = %q, want the current tunnel", status.URL)
	}
	if status.Error != "" {
		t.Fatalf("error = %q, want empty", status.Error)
	}
	if tun.cancel == nil {
		t.Fatal("旧事件清掉了当前代的 cancel")
	}
}

// TestSupervisorExitIgnoredAfterClose 验证 Close 之后到达的退出事件不会
// 把状态改回可重试。
func TestSupervisorExitIgnoredAfterClose(t *testing.T) {
	tun := &controller{closed: true, generation: 2}

	tun.supervisorExited(2, errors.New("late error"))

	status := tun.Status()
	if status.Error != "" || status.URL != "" {
		t.Fatalf("status = %+v, want untouched", status)
	}
}
