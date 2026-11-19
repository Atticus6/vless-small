package tunnel

import "testing"

func TestLocalTunnelPort(t *testing.T) {
	tests := []struct {
		listen string
		want   int
	}{
		{listen: ":8080", want: 8080},
		{listen: "0.0.0.0:9000", want: 9000},
		{listen: "[::]:8080", want: 8080},
		{listen: "127.0.0.1:8081", want: 8081},
		{listen: "localhost:8082", want: 8082},
	}

	for _, tt := range tests {
		got, err := localTunnelPort(tt.listen)
		if err != nil {
			t.Fatalf("localTunnelPort(%q): %v", tt.listen, err)
		}
		if got != tt.want {
			t.Fatalf("localTunnelPort(%q) = %d, want %d", tt.listen, got, tt.want)
		}
	}
}

func TestLocalTunnelPortRejectsInvalid(t *testing.T) {
	for _, listen := range []string{"", "8080", "nonsense", ":0", ":70000"} {
		if _, err := localTunnelPort(listen); err == nil {
			t.Fatalf("localTunnelPort(%q) succeeded, want error", listen)
		}
	}
}

func TestLocalTunnelHost(t *testing.T) {
	tests := []struct {
		listen string
		want   string
	}{
		// 通配地址回源到回环：服务在回环上也能收到。
		{listen: ":8080", want: "127.0.0.1"},
		{listen: "0.0.0.0:9000", want: "127.0.0.1"},
		{listen: "[::]:8080", want: "127.0.0.1"},
		// 显式绑定时必须回源到同一地址，否则隧道连不上服务。
		{listen: "127.0.0.1:8081", want: "127.0.0.1"},
		{listen: "192.168.1.10:8080", want: "192.168.1.10"},
		{listen: "localhost:8082", want: "localhost"},
		// IPv6 字面量必须带方括号，否则拼进 URL 会解析失败。
		{listen: "[::1]:8080", want: "[::1]"},
		{listen: "[fd7a:115c:a1e0::1]:8080", want: "[fd7a:115c:a1e0::1]"},
	}

	for _, tt := range tests {
		if got := localTunnelHost(tt.listen); got != tt.want {
			t.Fatalf("localTunnelHost(%q) = %q, want %q", tt.listen, got, tt.want)
		}
	}

	// 非法地址退化为回环，保持可用而不是拒绝。
	if got := localTunnelHost("nonsense"); got != "127.0.0.1" {
		t.Fatalf("localTunnelHost(nonsense) = %q, want 127.0.0.1", got)
	}
}

func TestTunnelControllerDisabledNoop(t *testing.T) {
	ctl := New(":8080")
	defer ctl.Close()

	ctl.Sync(Signal{})
	status := ctl.Status()
	if status.URL != "" {
		t.Fatalf("disabled tunnel URL = %q, want empty", status.URL)
	}
}
