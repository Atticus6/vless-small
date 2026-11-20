package quicktunnel

import (
	"net/netip"
	"testing"
)

// TestRotateAddrsCoversAllAddresses 验证地址数超过单次尝试上限时会轮转
// 起点：多轮重连必须能覆盖到所有地址，而不是永远只试前 8 个。
func TestRotateAddrsCoversAllAddresses(t *testing.T) {
	addrs := make([]netip.AddrPort, 0, 10)
	for i := 0; i < 10; i++ {
		addrs = append(addrs, netip.AddrPortFrom(
			netip.AddrFrom4([4]byte{192, 0, 2, byte(i + 1)}),
			7844,
		))
	}

	edgeDialCursor.Store(0)
	seen := make(map[netip.AddrPort]struct{})
	for round := 0; round < len(addrs); round++ {
		window := rotateAddrs(addrs, 8)
		if len(window) != 8 {
			t.Fatalf("window length = %d, want 8", len(window))
		}
		for _, addr := range window {
			seen[addr] = struct{}{}
		}
	}
	if len(seen) != len(addrs) {
		t.Fatalf("covered %d/%d addresses", len(seen), len(addrs))
	}

	if got := rotateAddrs(addrs, len(addrs)); len(got) != len(addrs) {
		t.Fatalf("limit >= len: got %d addresses", len(got))
	}
	if got := rotateAddrs(nil, 8); len(got) != 0 {
		t.Fatalf("empty addrs = %v", got)
	}
}
