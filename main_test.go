package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	srv := newServer(users, path)
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
	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "")
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
	ts := newTestWSServer(t, map[[16]byte]struct{}{id: {}}, "")
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
	if err := f.feed(conn, pkt); err != nil {
		t.Fatalf("feed: %v", err)
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
