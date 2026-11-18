package main

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

func TestTunnelControllerDisabledNoop(t *testing.T) {
	ctl := newTunnelController(":8080")
	defer ctl.Close()

	ctl.Sync(false)
	status := ctl.Status()
	if status.URL != "" {
		t.Fatalf("disabled tunnel URL = %q, want empty", status.URL)
	}
}
