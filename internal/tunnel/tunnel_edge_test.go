//go:build cloudflared

package tunnel

import (
	"context"
	"testing"
)

// 边缘位置只接受当前代次且未退出的隧道；过期的 supervisor 事件不能写状态。
func TestRecordEdgeLocation(t *testing.T) {
	ctl := New(":8080").(*controller)
	defer ctl.Close()

	// 未启动时 cancel == nil，任何位置都应被忽略。
	ctl.recordEdgeLocation(ctl.generation, "hkg")
	if got := ctl.Status().EdgeLocation; got != "" {
		t.Fatalf("edge location before start = %q, want empty", got)
	}

	// 模拟一次已启动的隧道。
	ctl.mu.Lock()
	ctl.generation++
	generation := ctl.generation
	_, cancel := context.WithCancel(context.Background())
	ctl.cancel = cancel
	ctl.mu.Unlock()
	defer cancel()

	ctl.recordEdgeLocation(generation, "hkg")
	if got := ctl.Status().EdgeLocation; got != "hkg" {
		t.Fatalf("edge location = %q, want hkg", got)
	}

	// 旧代次的事件必须丢弃。
	ctl.recordEdgeLocation(generation-1, "nrt")
	if got := ctl.Status().EdgeLocation; got != "hkg" {
		t.Fatalf("stale event overwrote location: %q", got)
	}

	// 空位置不覆盖已有值。
	ctl.recordEdgeLocation(generation, "")
	if got := ctl.Status().EdgeLocation; got != "hkg" {
		t.Fatalf("empty location overwrote value: %q", got)
	}
}
