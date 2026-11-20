package app

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/atticus6/vless-small/internal/cert"
	"github.com/atticus6/vless-small/internal/tunnel"
	"github.com/gorilla/websocket"
)

const testUUID = "14725836-1234-5678-9abc-def012345678"

func mustTestUUID(t *testing.T) [16]byte {
	t.Helper()
	id, err := parseUUID(testUUID)
	if err != nil {
		t.Fatalf("parseUUID: %v", err)
	}
	return id
}

// buildRequest 按 VLESS 头格式拼一个请求, 目标地址支持 IP 或域名.
func buildRequest(t *testing.T, id [16]byte, cmd byte, addr string, payload []byte) []byte {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("port %q: %v", portStr, err)
	}

	b := []byte{vlessVersion}
	b = append(b, id[:]...)
	b = append(b, 0) // addon 长度
	b = append(b, cmd)
	b = binary.BigEndian.AppendUint16(b, uint16(port))

	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			b = append(b, atypIPv4)
			b = append(b, ip4...)
		} else {
			b = append(b, atypIPv6)
			b = append(b, ip.To16()...)
		}
	} else {
		b = append(b, atypDomain, byte(len(host)))
		b = append(b, host...)
	}
	return append(b, payload...)
}

func newTestWSServer(t *testing.T, users map[[16]byte]struct{}, path string) *httptest.Server {
	t.Helper()
	srv := NewServer(users, path)
	ts := httptest.NewServer(http.HandlerFunc(srv.handleWS))
	t.Cleanup(ts.Close)
	return ts
}

func dialTestWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(ts.URL, "http")
	ws, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	t.Cleanup(func() { ws.Close() })
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	return ws
}

// readWSBytes 跨 WS 消息累计 n 字节: TCP 不保证一次 Read 对应一条消息.
func readWSBytes(t *testing.T, ws *websocket.Conn, n int) []byte {
	t.Helper()
	var out []byte
	for len(out) < n {
		_, data, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read ws: %v (got %d/%d bytes)", err, len(out), n)
		}
		out = append(out, data...)
	}
	if len(out) != n {
		t.Fatalf("read %d bytes, want %d", len(out), n)
	}
	return out
}

func startTCPEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String()
}

// startOneShotEcho 回显第一段数据后立即关闭，模拟请求-响应型目标。
func startOneShotEcho(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		buf := make([]byte, 1024)
		n, err := conn.Read(buf)
		if err != nil || n == 0 {
			return
		}
		_, _ = conn.Write(buf[:n])
	}()
	return ln.Addr().String()
}

func startChunkedTCP(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = conn.Write([]byte("pi"))
		time.Sleep(50 * time.Millisecond)
		_, _ = conn.Write([]byte("ng"))
		time.Sleep(50 * time.Millisecond)
	}()
	return ln.Addr().String()
}

func startUDPEcho(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			_, _ = pc.WriteTo(buf[:n], addr)
		}
	}()
	return pc.LocalAddr().String()
}

func TestParseRequest(t *testing.T) {
	id := mustTestUUID(t)
	header := buildRequest(t, id, cmdTCP, "example.com:443", []byte("early"))

	gotID, cmd, target, payload, err := parseRequest(header)
	if err != nil {
		t.Fatalf("parseRequest: %v", err)
	}
	if gotID != id {
		t.Fatalf("id = %x, want %x", gotID, id)
	}
	if cmd != cmdTCP {
		t.Fatalf("cmd = %d, want %d", cmd, cmdTCP)
	}
	if target != "example.com:443" {
		t.Fatalf("target = %q", target)
	}
	if string(payload) != "early" {
		t.Fatalf("payload = %q", payload)
	}

	if _, _, _, _, err := parseRequest([]byte{0, 1}); err == nil {
		t.Fatal("expected error for short header")
	}
}

func TestTCPRelay(t *testing.T) {
	id := mustTestUUID(t)
	echo := startTCPEcho(t)
	srv := NewServer(map[[16]byte]struct{}{id: {}}, "")
	ts := httptest.NewServer(http.HandlerFunc(srv.handleWS))
	t.Cleanup(ts.Close)
	ws := dialTestWS(t, ts)

	req := buildRequest(t, id, cmdTCP, echo, []byte("ping"))
	if err := ws.WriteMessage(websocket.BinaryMessage, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_, resp, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if !bytes.Equal(resp, []byte{vlessVersion, 0}) {
		t.Fatalf("response header = %v", resp)
	}

	got := readWSBytes(t, ws, len("ping"))
	if string(got) != "ping" {
		t.Fatalf("echoed early data = %q", got)
	}

	// 会话建立后的普通数据帧.
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("hello")); err != nil {
		t.Fatalf("write data: %v", err)
	}
	got = readWSBytes(t, ws, len("hello"))
	if string(got) != "hello" {
		t.Fatalf("echoed data = %q", got)
	}

	traffic := srv.takeTraffic()
	delta := traffic[id]
	if delta.Up != uint64(len("ping")+len("hello")) {
		t.Fatalf("upload bytes = %d, want %d", delta.Up, len("ping")+len("hello"))
	}
	if delta.Down != uint64(len("ping")+len("hello")) {
		t.Fatalf("download bytes = %d, want %d", delta.Down, len("ping")+len("hello"))
	}
}

// TestTCPRelayResponseHeaderBeforeDial 验证响应头在目标拨号之前发出：
// 目标不可达时客户端仍应立刻收到 [0,0]，而不是等拨号失败后才回。
//
// 这个顺序让客户端可以在节点解析/建连目标的同时发送请求 payload，
// 把“节点 → 目标”的建连 RTT 与客户端发包重叠；Xray 服务端也是同样顺序。
func TestTCPRelayResponseHeaderBeforeDial(t *testing.T) {
	id := mustTestUUID(t)

	// 先占用一个本地端口再释放，得到一个当前必然拒绝连接的地址，
	// 用它模拟“拨号失败”而不影响其他用例。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	target := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "")
	ws := dialTestWS(t, ts)

	req := buildRequest(t, id, cmdTCP, target, nil)
	if err := ws.WriteMessage(websocket.BinaryMessage, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_, resp, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read response header before dial: %v", err)
	}
	if !bytes.Equal(resp, []byte{vlessVersion, 0}) {
		t.Fatalf("response header = %v", resp)
	}

	// 拨号失败后服务端关闭会话，客户端应收到关闭而不是继续等待数据。
	// 读取超时说明服务端还挂着，不能当作“已关闭”放过。
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = ws.ReadMessage()
	if err == nil {
		t.Fatal("expected session to close after target dial failure")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("session stayed open after target dial failure: %v", err)
	}
}

// TestTCPRelayLargeMessage 覆盖单条 WS 消息远大于转发缓冲区的情况：
// 服务端必须按块流式转发，不能假设一条消息对应一次 Read。
func TestTCPRelayLargeMessage(t *testing.T) {
	id := mustTestUUID(t)
	echo := startTCPEcho(t)
	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "")
	ws := dialTestWS(t, ts)

	req := buildRequest(t, id, cmdTCP, echo, nil)
	if err := ws.WriteMessage(websocket.BinaryMessage, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_, resp, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if !bytes.Equal(resp, []byte{vlessVersion, 0}) {
		t.Fatalf("response header = %v", resp)
	}

	// 3 个缓冲区大小的数据放在一条 WS 消息里；回显会拆成多条消息返回。
	payload := bytes.Repeat([]byte("0123456789abcdef"), 3*bufferSize/16)
	if err := ws.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		t.Fatalf("write payload: %v", err)
	}
	got := readWSBytes(t, ws, len(payload))
	if !bytes.Equal(got, payload) {
		t.Fatal("large payload mismatch")
	}
}

func TestTCPRelayChunked(t *testing.T) {
	id := mustTestUUID(t)
	target := startChunkedTCP(t)
	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "")
	ws := dialTestWS(t, ts)

	req := buildRequest(t, id, cmdTCP, target, nil)
	if err := ws.WriteMessage(websocket.BinaryMessage, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_, resp, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if !bytes.Equal(resp, []byte{vlessVersion, 0}) {
		t.Fatalf("response header = %v", resp)
	}

	got := readWSBytes(t, ws, 4)
	if string(got) != "ping" {
		t.Fatalf("chunked echo = %q", got)
	}
}

func TestUDPRelay(t *testing.T) {
	id := mustTestUUID(t)
	echo := startUDPEcho(t)
	srv := NewServer(map[[16]byte]struct{}{id: {}}, "")
	ts := httptest.NewServer(http.HandlerFunc(srv.handleWS))
	t.Cleanup(ts.Close)
	ws := dialTestWS(t, ts)

	body := []byte("dns")
	pkt := binary.BigEndian.AppendUint16(nil, uint16(len(body)))
	pkt = append(pkt, body...)

	req := buildRequest(t, id, cmdUDP, echo, pkt)
	if err := ws.WriteMessage(websocket.BinaryMessage, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_, resp, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if !bytes.Equal(resp, []byte{vlessVersion, 0}) {
		t.Fatalf("response header = %v", resp)
	}

	_, resp, err = ws.ReadMessage()
	if err != nil {
		t.Fatalf("read udp response: %v", err)
	}
	if len(resp) < 2 {
		t.Fatalf("udp response too short: %d", len(resp))
	}
	n := int(binary.BigEndian.Uint16(resp[:2]))
	if n != len(body) || !bytes.Equal(resp[2:2+n], body) {
		t.Fatalf("udp response = %q", resp)
	}

	traffic := srv.takeTraffic()
	delta := traffic[id]
	if delta.Up != uint64(len(body)) || delta.Down != uint64(len(body)) {
		t.Fatalf("udp traffic = %+v, want %d/%d", delta, len(body), len(body))
	}
}

func TestUDPRelaySplitPacket(t *testing.T) {
	id := mustTestUUID(t)
	echo := startUDPEcho(t)
	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "")
	ws := dialTestWS(t, ts)

	body := []byte("abc")
	pkt := binary.BigEndian.AppendUint16(nil, uint16(len(body)))
	pkt = append(pkt, body...)

	// 第一条消息只给长度头和一个字节, 剩下的留到下一帧.
	const split = 3
	req := buildRequest(t, id, cmdUDP, echo, pkt[:split])
	if err := ws.WriteMessage(websocket.BinaryMessage, req); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_, resp, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read response header: %v", err)
	}
	if !bytes.Equal(resp, []byte{vlessVersion, 0}) {
		t.Fatalf("response header = %v", resp)
	}

	if err := ws.WriteMessage(websocket.BinaryMessage, pkt[split:]); err != nil {
		t.Fatalf("write rest of packet: %v", err)
	}

	_, resp, err = ws.ReadMessage()
	if err != nil {
		t.Fatalf("read udp response: %v", err)
	}
	if len(resp) < 2 {
		t.Fatalf("udp response too short: %d", len(resp))
	}
	n := int(binary.BigEndian.Uint16(resp[:2]))
	if n != len(body) || !bytes.Equal(resp[2:2+n], body) {
		t.Fatalf("udp response = %q", resp)
	}
}

func TestRejectUnknownUUID(t *testing.T) {
	id := mustTestUUID(t)
	other, err := parseUUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	if other == id {
		t.Fatal("test UUIDs must differ")
	}

	echo := startTCPEcho(t)
	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "")
	ws := dialTestWS(t, ts)

	req := buildRequest(t, other, cmdTCP, echo, nil)
	if err := ws.WriteMessage(websocket.BinaryMessage, req); err != nil {
		t.Fatalf("write request: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = ws.ReadMessage()
	if err == nil {
		t.Fatal("expected connection to be closed for unknown UUID")
	}
	var nerr net.Error
	if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatalf("server kept connection open until read timeout: %v", err)
	}
}

type fakeConn struct {
	readDeadline time.Time
	written      bytes.Buffer
}

func (c *fakeConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error)      { return c.written.Write(p) }
func (c *fakeConn) Close() error                     { return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr             { return fakeAddr{} }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
func (c *fakeConn) SetReadDeadline(t time.Time) error {
	c.readDeadline = t
	return nil
}

type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

func TestUDPFramerExtendsIdleDeadline(t *testing.T) {
	conn := &fakeConn{readDeadline: time.Now().Add(-time.Hour)}
	f := &udpFramer{}

	pkt := binary.BigEndian.AppendUint16(nil, 3)
	pkt = append(pkt, "abc"...)
	written, err := f.feed(conn, pkt)
	if err != nil {
		t.Fatalf("feed: %v", err)
	}
	if written != 3 {
		t.Fatalf("written = %d, want 3", written)
	}
	if !conn.readDeadline.After(time.Now()) {
		t.Fatalf("read deadline not extended: %v", conn.readDeadline)
	}
	if conn.written.String() != "abc" {
		t.Fatalf("forwarded = %q", conn.written.String())
	}
}

func TestPathMismatch(t *testing.T) {
	id := mustTestUUID(t)
	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "/ws")

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/other"
	_, resp, err := websocket.DefaultDialer.Dial(url, nil)
	if err == nil {
		// Dial 成功时服务端一定不该升级; 关闭兜底连接.
		t.Fatal("expected dial to fail on wrong path")
	}
	if resp == nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %v, want 404", resp)
	}
}

func TestParseNodeConfig(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    NodeConfig
		wantErr bool
	}{
		{name: "empty", raw: ""},
		{
			name: "valid",
			raw:  "https://dsh.atticus.eu.org_hk_abc123",
			want: NodeConfig{BaseURL: "https://dsh.atticus.eu.org", Name: "hk", Token: "abc123"},
		},
		{name: "missing token", raw: "https://dsh.atticus.eu.org_hk", wantErr: true},
		{name: "bad scheme", raw: "ftp://dsh.atticus.eu.org_hk_abc123", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseNodeConfig(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseNodeConfig: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestSyncDevices(t *testing.T) {
	enabledID := mustTestUUID(t)
	disabledID, err := parseUUID("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	if err != nil {
		t.Fatal(err)
	}
	staticID, err := parseUUID("11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/nodes/report" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")

		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode report: %v", err)
			return
		}
		if body["name"] != "hk" || body["runtime"] != "vless-small" {
			t.Errorf("report body = %#v", body)
		}
		if body["version"] != Version {
			t.Errorf("report version = %v, want %q", body["version"], Version)
		}
		if body["os"] == "" || body["arch"] == "" {
			t.Errorf("report os/arch missing: %#v", body)
		}
		// tsnet 支持已移除，上报里不应再出现相关字段。
		for _, key := range []string{"tsnetSupported", "tsnetIp"} {
			if _, ok := body[key]; ok {
				t.Errorf("report should not contain %s: %#v", key, body)
			}
		}
		if _, ok := body["tunnel"].(map[string]any); !ok {
			t.Errorf("report tunnel missing: %#v", body["tunnel"])
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"devices": []map[string]any{
				{"id": testUUID, "name": "phone", "enabled": true},
				{"id": "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", "name": "disabled", "enabled": false},
			},
			"update": map[string]any{
				"version": "v9.9.9",
				"url":     "https://example.com/api/nodes/update?name=hk",
				"sha256":  "deadbeef",
			},
			"tunnel": map[string]any{"enabled": true},
		})
	}))
	defer ts.Close()

	srv := NewServer(nil, "")
	cfg := NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"}
	static := map[[16]byte]struct{}{staticID: {}}

	result, err := syncDevices(
		cfg, srv, ":8080", "/ws", static, nodeInfo{runtime: "vless-small"},
		tunnel.Report{}, cert.Report{},
	)
	if err != nil {
		t.Fatalf("syncDevices: %v", err)
	}
	if result == nil || result.Update == nil || result.Update.Version != "v9.9.9" {
		t.Fatalf("result = %#v, want update v9.9.9", result)
	}
	if result.Tunnel == nil || !result.Tunnel.Enabled {
		t.Fatalf("result.Tunnel = %#v, want enabled", result.Tunnel)
	}
	if gotAuth != "Bearer secret" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if !srv.hasUser(enabledID) {
		t.Fatal("enabled device missing")
	}
	if !srv.hasUser(staticID) {
		t.Fatal("static UUID missing")
	}
	if srv.hasUser(disabledID) {
		t.Fatal("disabled device should be skipped")
	}
	if srv.UserCount() != 2 {
		t.Fatalf("userCount = %d, want 2", srv.UserCount())
	}
}

func TestDetectRailwayAndHosts(t *testing.T) {
	for _, key := range []string{
		"NODE_RUNTIME", "RUNTIME",
		"PUBLIC_HOST", "PUBLIC_HOSTS",
		"RAILWAY_ENVIRONMENT", "RAILWAY_PUBLIC_DOMAIN",
		"RENDER", "RENDER_EXTERNAL_URL", "RENDER_EXTERNAL_HOSTNAME",
		"FLY_APP_NAME",
		"KOYEB_APP_NAME", "KOYEB_PUBLIC_DOMAIN",
		"CF_PAGES", "CF_PAGES_URL",
		"ZEABUR", "ZEABUR_WEB_URL",
		"NF_HOSTS", "NF_PLAN_ID",
		"NORTHFLANK", "NORTHFLANK_SERVICE_ID", "NORTHFLANK_PROJECT_ID",
		"NORTHFLANK_PUBLIC_URL", "NORTHFLANK_SERVICE_URL", "SERVICE_URL",
		"PUBLIC_URL",
	} {
		t.Setenv(key, "")
	}

	t.Setenv("RAILWAY_ENVIRONMENT", "production")
	t.Setenv("RAILWAY_PUBLIC_DOMAIN", "vless.up.railway.app")

	if got := detectRuntime(); got != "railway" {
		t.Fatalf("runtime = %q, want railway", got)
	}
	hosts := detectHosts()
	if len(hosts) != 1 || hosts[0] != "vless.up.railway.app" {
		t.Fatalf("hosts = %v, want [vless.up.railway.app]", hosts)
	}
}

func TestDetectNorthflank(t *testing.T) {
	for _, key := range []string{
		"NODE_RUNTIME", "RUNTIME",
		"PUBLIC_HOST", "PUBLIC_HOSTS",
		"RAILWAY_ENVIRONMENT", "RAILWAY_PUBLIC_DOMAIN",
		"RENDER", "RENDER_EXTERNAL_URL", "RENDER_EXTERNAL_HOSTNAME",
		"FLY_APP_NAME",
		"KOYEB_APP_NAME", "KOYEB_PUBLIC_DOMAIN",
		"CF_PAGES", "CF_PAGES_URL",
		"ZEABUR", "ZEABUR_WEB_URL",
		"NF_HOSTS", "NF_PLAN_ID",
		"SERVICE_URL", "PUBLIC_URL",
	} {
		t.Setenv(key, "")
	}

	t.Setenv("NF_PLAN_ID", "plan-123")
	t.Setenv("NF_HOSTS", "https://p01--vless--abc.code.run, p01--vless--def.code.run")

	if got := detectRuntime(); got != "northflank" {
		t.Fatalf("runtime = %q, want northflank", got)
	}
	hosts := detectHosts()
	want := []string{"p01--vless--abc.code.run", "p01--vless--def.code.run"}
	if len(hosts) != len(want) || hosts[0] != want[0] || hosts[1] != want[1] {
		t.Fatalf("hosts = %v, want %v", hosts, want)
	}
}

func TestIsAutoSleepRuntime(t *testing.T) {
	for _, runtime := range []string{"railway"} {
		if !isAutoSleepRuntime(runtime) {
			t.Fatalf("%s should disable background sync", runtime)
		}
	}
	for _, runtime := range []string{"local", "render", "fly", "koyeb", "cloudflare", "zeabur", "northflank"} {
		if isAutoSleepRuntime(runtime) {
			t.Fatalf("%s should keep background sync", runtime)
		}
	}
}

func TestLookupIPInfo(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ip":      "1.2.3.4",
			"country": "HK",
			"loc":     "22.3,114.2",
		})
	}))
	defer ts.Close()

	t.Setenv("IP_LOOKUP_URL", ts.URL)

	info, err := lookupIPInfo()
	if err != nil {
		t.Fatalf("lookupIPInfo: %v", err)
	}
	if info.ipv4 != "1.2.3.4" {
		t.Fatalf("ipv4 = %q", info.ipv4)
	}
	if info.countryCode != "HK" {
		t.Fatalf("countryCode = %q", info.countryCode)
	}
	if info.latitude == nil || info.longitude == nil {
		t.Fatal("latitude/longitude missing")
	}
	if *info.latitude != 22.3 || *info.longitude != 114.2 {
		t.Fatalf("loc = %v,%v", *info.latitude, *info.longitude)
	}
}

func TestHealthEndpoints(t *testing.T) {
	ts := newTestWSServer(t, nil, "/ws")

	rootResp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	rootBody, err := io.ReadAll(rootResp.Body)
	rootResp.Body.Close()
	if err != nil {
		t.Fatalf("read /: %v", err)
	}
	if rootResp.StatusCode != http.StatusOK {
		t.Fatalf("/ status = %d, want 200", rootResp.StatusCode)
	}
	if string(rootBody) != "vless-small ok\n" {
		t.Fatalf("/ body = %q", rootBody)
	}

	healthResp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", healthResp.StatusCode)
	}
	if !strings.Contains(healthResp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("/healthz content-type = %q", healthResp.Header.Get("Content-Type"))
	}

	var health healthResponse
	if err := json.NewDecoder(healthResp.Body).Decode(&health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if !health.OK {
		t.Fatal("/healthz ok = false")
	}
	if health.Uptime == "" {
		t.Fatal("/healthz uptime is empty")
	}
	if health.UptimeSeconds < 0 {
		t.Fatalf("/healthz uptimeSeconds = %d", health.UptimeSeconds)
	}

	wsResp, err := http.Get(ts.URL + "/ws")
	if err != nil {
		t.Fatalf("GET /ws: %v", err)
	}
	defer wsResp.Body.Close()
	if wsResp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("/ws status = %d, want 426", wsResp.StatusCode)
	}
}

// TestSyncDevicesTunnelResetOnce 验证进程启动后的首份成功上报会带
// tunnelReset，让主控清掉上一个进程遗留的隧道地址；成功后不再重复发送。
func TestSyncDevicesTunnelResetOnce(t *testing.T) {
	var mu sync.Mutex
	var resets []bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		resets = append(resets, body["tunnelReset"] == true)
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": []any{}})
	}))
	defer ts.Close()

	srv := NewServer(nil, "")
	cfg := NodeConfig{BaseURL: ts.URL, Name: "hk", Token: "secret"}
	reset := &atomic.Bool{}
	reset.Store(true)

	for i := 0; i < 2; i++ {
		if _, err := syncDevicesWithReset(
			cfg, srv, ":8080", "/ws", nil,
			nodeInfo{runtime: "vless-small"},
			tunnel.Report{}, cert.Report{}, reset,
		); err != nil {
			t.Fatalf("sync %d: %v", i, err)
		}
	}

	mu.Lock()
	got := append([]bool(nil), resets...)
	mu.Unlock()
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("tunnelReset flags = %v, want [true false]", got)
	}
	if reset.Load() {
		t.Fatal("reset flag 未在成功上报后清除")
	}
}
