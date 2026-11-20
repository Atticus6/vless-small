package hy2

import (
	"context"
	"net"
	"testing"
)

func TestProbeReachabilitySkipsLoopback(t *testing.T) {
	result := ProbeReachability(context.Background(), "127.0.0.1:8081", "1.2.3.4")
	if result.Reachable {
		t.Fatal("loopback listen must not be reachable")
	}
	if result.Reason == "" {
		t.Fatal("expected a reason")
	}
}

func TestProbeReachabilityRejectsPrivateIP(t *testing.T) {
	for _, ip := range []string{"10.0.0.1", "192.168.1.5", "172.16.0.1", "100.64.0.1", "127.0.0.1", ""} {
		result := ProbeReachability(context.Background(), ":8081", ip)
		if result.Reachable {
			t.Fatalf("%q: private/empty IP must not be reachable", ip)
		}
	}
}

func TestIsPublicIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{ip: "1.1.1.1", want: true},
		{ip: "203.0.113.5", want: true},
		{ip: "2001:db8::1", want: true},
		{ip: "10.0.0.1", want: false},
		{ip: "192.168.0.1", want: false},
		{ip: "172.16.5.4", want: false},
		{ip: "100.64.1.1", want: false},
		{ip: "127.0.0.1", want: false},
		{ip: "169.254.1.1", want: false},
		{ip: "::1", want: false},
	}
	for _, tt := range tests {
		ip := net.ParseIP(tt.ip)
		if got := isPublicIP(ip); got != tt.want {
			t.Fatalf("isPublicIP(%s) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestSameEndpoint(t *testing.T) {
	if !sameEndpoint("1.2.3.4:8081", "1.2.3.4:8081") {
		t.Fatal("identical endpoints should match")
	}
	if !sameEndpoint("[::1]:8081", "[0:0:0:0:0:0:0:1]:8081") {
		t.Fatal("IPv6 forms should be equivalent")
	}
	if sameEndpoint("1.2.3.4:8081", "1.2.3.4:8082") {
		t.Fatal("different ports must not match")
	}
	if sameEndpoint("1.2.3.4:8081", "1.2.3.5:8081") {
		t.Fatal("different IPs must not match")
	}
}

func TestParseSTUNResponseRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		msg  []byte
	}{
		{name: "empty", msg: nil},
		{name: "short", msg: make([]byte, 10)},
	}
	for _, tt := range tests {
		if _, err := parseSTUNResponse(tt.msg, make([]byte, 12)); err == nil {
			t.Fatalf("%s: expected error", tt.name)
		}
	}
}

func TestBuildSTUNRequest(t *testing.T) {
	msg, txID, err := buildSTUNRequest()
	if err != nil {
		t.Fatalf("buildSTUNRequest: %v", err)
	}
	if len(msg) != stunHeaderSize || len(txID) != 12 {
		t.Fatalf("sizes = %d/%d", len(msg), len(txID))
	}
	if string(msg[8:]) != string(txID) {
		t.Fatal("transaction ID not embedded")
	}
}
