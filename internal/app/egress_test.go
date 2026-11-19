package app

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/atticus6/vless-small/internal/cert"
	"github.com/atticus6/vless-small/internal/tunnel"
	"github.com/gorilla/websocket"
)

func TestBuildEgressAddresses(t *testing.T) {
	ips := []net.IP{
		net.ParseIP("10.0.0.185"),
		net.ParseIP("10.0.0.139"),
		net.ParseIP("10.0.0.61"),
		net.ParseIP("10.0.0.152"),
		net.ParseIP("10.0.0.208"),
		net.ParseIP("10.0.0.139"), // 重复地址应被去掉
	}

	got := buildEgressAddresses(ips, net.ParseIP("10.0.0.61"))
	want := []string{
		"10.0.0.61",
		"10.0.0.139",
		"10.0.0.152",
		"10.0.0.185",
		"10.0.0.208",
	}
	if len(got) != len(want) {
		t.Fatalf("addresses = %+v, want %d entries", got, len(want))
	}
	for index, address := range got {
		if address.IP != want[index] {
			t.Fatalf("addresses[%d].IP = %q, want %q", index, address.IP, want[index])
		}
		if address.Path != "/ip"+strconv.Itoa(index+1) {
			t.Fatalf("addresses[%d].Path = %q", index, address.Path)
		}
		if address.Default != (index == 0) {
			t.Fatalf("addresses[%d].Default = %v", index, address.Default)
		}
	}
}

func TestBuildEgressAddressesPrependsDefault(t *testing.T) {
	got := buildEgressAddresses(
		[]net.IP{net.ParseIP("10.0.0.2")},
		net.ParseIP("10.0.0.1"),
	)
	if len(got) != 2 || got[0].IP != "10.0.0.1" || !got[0].Default {
		t.Fatalf("addresses = %+v, want default 10.0.0.1 first", got)
	}
}

func TestEgressPathIndex(t *testing.T) {
	tests := []struct {
		path  string
		index int
		ok    bool
	}{
		{path: "/ip1", index: 1, ok: true},
		{path: "/ip2", index: 2, ok: true},
		{path: "/ip10", index: 10, ok: true},
		{path: "/ip0"},
		{path: "/ip01"},
		{path: "/ip"},
		{path: "/ipx"},
		{path: "/ip1/"},
		{path: "/ws"},
	}

	for _, test := range tests {
		index, ok := egressPathIndex(test.path)
		if index != test.index || ok != test.ok {
			t.Fatalf(
				"egressPathIndex(%q) = (%d, %v), want (%d, %v)",
				test.path, index, ok, test.index, test.ok,
			)
		}
	}
}

func TestDialNetwork(t *testing.T) {
	if got := dialNetwork("tcp", nil); got != "tcp" {
		t.Fatalf("dialNetwork(tcp, nil) = %q", got)
	}
	if got := dialNetwork("udp", nil); got != "udp" {
		t.Fatalf("dialNetwork(udp, nil) = %q", got)
	}
	source := net.ParseIP("10.0.0.2")
	if got := dialNetwork("tcp", source); got != "tcp4" {
		t.Fatalf("dialNetwork(tcp, source) = %q", got)
	}
	if got := dialNetwork("udp", source); got != "udp4" {
		t.Fatalf("dialNetwork(udp, source) = %q", got)
	}
}

func TestParseDefaultRouteInterface(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name: "选择默认路由",
			content: `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0100000A	0003	0	0	100	00000000	0	0	0
eth0	0000A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`,
			want: "eth0",
		},
		{
			name: "多条默认路由取 Metric 最小",
			content: `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	00000000	0100000A	0003	0	0	200	00000000	0	0	0
wlan0	00000000	0100000A	0003	0	0	100	00000000	0	0	0
`,
			want: "wlan0",
		},
		{
			name: "没有默认路由",
			content: `Iface	Destination	Gateway 	Flags	RefCnt	Use	Metric	Mask		MTU	Window	IRTT
eth0	0000A8C0	00000000	0001	0	0	0	00FFFFFF	0	0	0
`,
			want: "",
		},
		{
			name:    "空内容",
			content: "",
			want:    "",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := parseDefaultRouteInterface(strings.NewReader(test.content))
			if got != test.want {
				t.Fatalf("interface = %q, want %q", got, test.want)
			}
		})
	}
}

func TestSourceIPForPath(t *testing.T) {
	srv := NewServer(nil, "/ws")
	// 覆盖自动检测结果，保证测试不依赖运行机器的网卡。
	srv.egress = newEgressSelector([]egressAddress{
		{IP: "10.0.0.61", Path: "/ip1", Default: true},
		{IP: "10.0.0.139", Path: "/ip2"},
	})

	// 默认关闭：/ip2 按普通路径处理，由 -path 规则决定（这里是 404）。
	if ip, ok := srv.sourceIPForPath("/ip2"); !ok || ip != nil {
		t.Fatalf("disabled /ip2 = (%v, %v), want (nil, true)", ip, ok)
	}

	srv.SetEgressEnabled(true)
	if ip, ok := srv.sourceIPForPath("/ip1"); !ok || ip == nil || ip.String() != "10.0.0.61" {
		t.Fatalf("enabled /ip1 = (%v, %v)", ip, ok)
	}
	if ip, ok := srv.sourceIPForPath("/ip2"); !ok || ip == nil || ip.String() != "10.0.0.139" {
		t.Fatalf("enabled /ip2 = (%v, %v)", ip, ok)
	}
	if ip, ok := srv.sourceIPForPath("/ip3"); ok || ip != nil {
		t.Fatalf("enabled /ip3 = (%v, %v), want 404", ip, ok)
	}
	if ip, ok := srv.sourceIPForPath("/ws"); !ok || ip != nil {
		t.Fatalf("enabled /ws = (%v, %v), want default", ip, ok)
	}
}

func TestEgressWebSocketPaths(t *testing.T) {
	srv := NewServer(nil, "/ws")
	srv.egress = newEgressSelector([]egressAddress{
		{IP: "10.0.0.61", Path: "/ip1", Default: true},
		{IP: "10.0.0.139", Path: "/ip2"},
	})

	ts := httptest.NewServer(http.HandlerFunc(srv.handleWS))
	defer ts.Close()
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")

	dial := func(path string) (*websocket.Conn, error) {
		conn, _, err := websocket.DefaultDialer.Dial(wsURL+path, nil)
		return conn, err
	}

	// 开关默认关闭：/ip2 仍按 -path 规则拒绝。
	if conn, err := dial("/ip2"); err == nil {
		conn.Close()
		t.Fatal("/ip2 should be rejected while egress is disabled")
	}

	srv.SetEgressEnabled(true)
	if conn, err := dial("/ip2"); err != nil {
		t.Fatalf("/ip2 should be accepted: %v", err)
	} else {
		conn.Close()
	}
	if conn, err := dial("/ws"); err != nil {
		t.Fatalf("/ws should still be accepted: %v", err)
	} else {
		conn.Close()
	}
	if conn, err := dial("/ip3"); err == nil {
		conn.Close()
		t.Fatal("/ip3 should be rejected when no such egress address exists")
	}
}

func TestSyncDevicesReportsEgress(t *testing.T) {
	var report nodeReport
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Errorf("decode report: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
	}))
	defer ts.Close()

	srv := NewServer(nil, "")
	srv.egress = newEgressSelector([]egressAddress{
		{IP: "10.0.0.61", Path: "/ip1", Default: true},
		{IP: "10.0.0.139", Path: "/ip2"},
	})
	srv.SetEgressEnabled(true)

	if _, err := syncDevices(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv, ":8080", "/ws", nil,
		nodeInfo{runtime: "local"},
		tunnel.Report{}, cert.Report{},
	); err != nil {
		t.Fatalf("syncDevices: %v", err)
	}

	if report.Egress == nil || !report.Egress.Enabled {
		t.Fatalf("egress report = %+v, want enabled", report.Egress)
	}
	if len(report.Egress.Addresses) != 2 ||
		report.Egress.Addresses[0].Path != "/ip1" ||
		report.Egress.Addresses[0].IP != "10.0.0.61" {
		t.Fatalf("egress addresses = %+v", report.Egress.Addresses)
	}
}

func TestSyncDevicesReportsEmptyEgress(t *testing.T) {
	var report nodeReport
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Errorf("decode report: %v", err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
	}))
	defer ts.Close()

	srv := NewServer(nil, "")
	srv.egress = nil

	if _, err := syncDevices(
		NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"},
		srv, ":8080", "/ws", nil,
		nodeInfo{runtime: "railway"},
		tunnel.Report{}, cert.Report{},
	); err != nil {
		t.Fatalf("syncDevices: %v", err)
	}

	if report.Egress == nil || report.Egress.Enabled {
		t.Fatalf("egress report = %+v, want disabled", report.Egress)
	}
	if report.Egress.Addresses == nil || len(report.Egress.Addresses) != 0 {
		t.Fatalf("egress addresses = %+v, want empty array", report.Egress.Addresses)
	}
}

func TestConfigEndpointIncludesEgress(t *testing.T) {
	srv := NewServer(nil, "/ws")
	srv.egress = newEgressSelector([]egressAddress{
		{IP: "10.0.0.61", Path: "/ip1", Default: true},
		{IP: "10.0.0.139", Path: "/ip2"},
	})
	srv.SetEgressEnabled(true)
	srv.controlToken = "secret"

	req := httptest.NewRequest(http.MethodGet, "/secret/config", nil)
	rec := httptest.NewRecorder()
	if !srv.handleHealth(rec, req) {
		t.Fatal("config path was not recognized")
	}

	var got configResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode config: %v", err)
	}
	if !got.EgressEnabled {
		t.Fatal("egressEnabled = false, want true")
	}
	if len(got.Egress) != 2 || got.Egress[0].Path != "/ip1" {
		t.Fatalf("egress = %+v", got.Egress)
	}
}

func TestDialOutboundBindsSourceIPv4(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("loopback alias binding is only asserted on Linux")
	}

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	remote := make(chan string, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		remote <- conn.RemoteAddr().String()
		_ = conn.Close()
	}()

	conn, err := dialOutbound(
		"tcp",
		listener.Addr().String(),
		net.ParseIP("127.0.0.2"),
	)
	if err != nil {
		t.Skipf("loopback alias 127.0.0.2 unavailable: %v", err)
	}
	defer conn.Close()

	select {
	case address := <-remote:
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			t.Fatal(err)
		}
		if host != "127.0.0.2" {
			t.Fatalf("remote host = %q, want 127.0.0.2", host)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("listener did not accept connection")
	}
}
