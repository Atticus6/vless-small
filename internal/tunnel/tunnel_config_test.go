package tunnel

import "testing"

func TestTunnelHAConnections(t *testing.T) {
	t.Setenv("TUNNEL_HA_CONNECTIONS", "")
	if got := tunnelHAConnections(); got != tunnelDefaultHAConnections {
		t.Fatalf("default = %d", got)
	}

	t.Setenv("TUNNEL_HA_CONNECTIONS", "2")
	if got := tunnelHAConnections(); got != 2 {
		t.Fatalf("configured = %d, want 2", got)
	}

	t.Setenv("TUNNEL_HA_CONNECTIONS", "invalid")
	if got := tunnelHAConnections(); got != tunnelDefaultHAConnections {
		t.Fatalf("invalid = %d, want default", got)
	}

	t.Setenv("TUNNEL_HA_CONNECTIONS", "99")
	if got := tunnelHAConnections(); got != 4 {
		t.Fatalf("clamped = %d, want 4", got)
	}
}

func TestTunnelProtocol(t *testing.T) {
	t.Setenv("TUNNEL_PROTOCOL", "")
	if got := tunnelProtocol(); got != "auto" {
		t.Fatalf("default = %q", got)
	}

	t.Setenv("TUNNEL_PROTOCOL", "HTTP2")
	if got := tunnelProtocol(); got != "http2" {
		t.Fatalf("http2 = %q", got)
	}

	t.Setenv("TUNNEL_PROTOCOL", "quic")
	if got := tunnelProtocol(); got != "quic" {
		t.Fatalf("quic = %q", got)
	}

	t.Setenv("TUNNEL_PROTOCOL", "invalid")
	if got := tunnelProtocol(); got != "auto" {
		t.Fatalf("invalid = %q, want auto", got)
	}
}

func TestTunnelLogLevel(t *testing.T) {
	t.Setenv("TUNNEL_LOG_LEVEL", "")
	if got := tunnelLogLevel(); got != "error" {
		t.Fatalf("default = %q", got)
	}

	t.Setenv("TUNNEL_LOG_LEVEL", "debug")
	if got := tunnelLogLevel(); got != "debug" {
		t.Fatalf("debug = %q", got)
	}

	t.Setenv("TUNNEL_LOG_LEVEL", "invalid")
	if got := tunnelLogLevel(); got != "error" {
		t.Fatalf("invalid = %q, want error", got)
	}
}

func TestTunnelQUICFlowLimits(t *testing.T) {
	t.Setenv("TUNNEL_QUIC_CONN_FLOW_MIB", "")
	t.Setenv("TUNNEL_QUIC_STREAM_FLOW_MIB", "")
	conn, stream := tunnelQUICFlowLimits()
	if conn != tunnelQUICConnFlowLimit || stream != tunnelQUICStreamFlowLimit {
		t.Fatalf("defaults = %d/%d", conn, stream)
	}

	t.Setenv("TUNNEL_QUIC_CONN_FLOW_MIB", "16")
	t.Setenv("TUNNEL_QUIC_STREAM_FLOW_MIB", "4")
	conn, stream = tunnelQUICFlowLimits()
	if conn != 16<<20 || stream != 4<<20 {
		t.Fatalf("configured = %d/%d", conn, stream)
	}

	t.Setenv("TUNNEL_QUIC_CONN_FLOW_MIB", "0")
	t.Setenv("TUNNEL_QUIC_STREAM_FLOW_MIB", "999")
	conn, stream = tunnelQUICFlowLimits()
	if conn != tunnelQUICConnFlowLimit || stream != tunnelQUICStreamFlowLimit {
		t.Fatalf("invalid = %d/%d, want defaults", conn, stream)
	}
}
