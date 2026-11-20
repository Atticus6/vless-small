package quicktunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type connectResponse struct {
	status  int
	headers http.Header
	errText string
}

func readTestConnectResponse(t *testing.T, rw io.Reader) connectResponse {
	t.Helper()
	if err := readDataPreamble(rw); err != nil {
		t.Fatalf("read preamble: %v", err)
	}
	msg, err := readMessage(rw)
	if err != nil {
		t.Fatalf("read response message: %v", err)
	}
	root, err := msg.rootStruct()
	if err != nil {
		t.Fatal(err)
	}
	out := connectResponse{headers: make(http.Header)}
	out.errText, _ = msg.textAt(root, 0)
	seg, ptrWord, err := msg.ptrAt(root, 1)
	if err != nil {
		return out
	}
	list, err := msg.listAt(seg, ptrWord)
	if err != nil {
		return out
	}
	for i := 0; i < list.elemCount; i++ {
		entry, err := msg.compositeElem(list, i)
		if err != nil {
			continue
		}
		key, _ := msg.textAt(entry, 0)
		value, _ := msg.textAt(entry, 1)
		switch {
		case key == metadataHTTPStatus:
			fmt.Sscanf(value, "%d", &out.status)
		case strings.HasPrefix(key, metadataHeader):
			out.headers.Add(strings.TrimPrefix(key, metadataHeader), value)
		}
	}
	return out
}

func startTestStream(t *testing.T, localURL string) (net.Conn, *Client, context.CancelFunc, <-chan struct{}) {
	t.Helper()
	client := New(Config{LocalURL: localURL, Logf: t.Logf})
	clientSide, serverSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		client.handleDataStream(ctx, serverSide)
	}()
	return clientSide, client, cancel, done
}

func writeTestRequest(t *testing.T, conn net.Conn, frame []byte) <-chan error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() {
		if err := writeDataPreamble(conn); err != nil {
			errCh <- err
			return
		}
		_, err := conn.Write(frame)
		errCh <- err
	}()
	return errCh
}

func TestProxyHTTPGet(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "demo.trycloudflare.com" {
			t.Errorf("host = %q", r.Host)
		}
		w.Header().Set("X-Origin", "yes")
		_, _ = fmt.Fprintf(w, "hello %s", r.URL.Path)
	}))
	defer origin.Close()

	conn, _, cancel, done := startTestStream(t, origin.URL)
	defer cancel()
	defer conn.Close()

	frame := buildConnectRequest("https://demo.trycloudflare.com/ws", connectionTypeHTTP, []metadata{
		{Key: metadataMethod, Val: "GET"},
		{Key: metadataHost, Val: "demo.trycloudflare.com"},
	})
	if err := <-writeTestRequest(t, conn, frame); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp := readTestConnectResponse(t, conn)
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d (err=%q)", resp.status, resp.errText)
	}
	if resp.headers.Get("X-Origin") != "yes" {
		t.Fatalf("headers = %v", resp.headers)
	}
	body, err := io.ReadAll(conn)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(body) != "hello /ws" {
		t.Fatalf("body = %q", body)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not exit")
	}
}

func TestProxyHTTPPostBody(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _ = fmt.Fprintf(w, "got:%s", body)
	}))
	defer origin.Close()

	conn, _, cancel, done := startTestStream(t, origin.URL)
	defer cancel()
	defer conn.Close()

	frame := buildConnectRequest("https://demo.trycloudflare.com/api", connectionTypeHTTP, []metadata{
		{Key: metadataMethod, Val: "POST"},
		{Key: metadataHost, Val: "demo.trycloudflare.com"},
		{Key: metadataHeader + "Content-Length", Val: "5"},
	})
	go func() {
		_ = writeDataPreamble(conn)
		_, _ = conn.Write(frame)
		_, _ = conn.Write([]byte("hello"))
	}()

	resp := readTestConnectResponse(t, conn)
	if resp.status != http.StatusOK {
		t.Fatalf("status = %d (err=%q)", resp.status, resp.errText)
	}
	body, _ := io.ReadAll(conn)
	if string(body) != "got:hello" {
		t.Fatalf("body = %q", body)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not exit")
	}
}

func TestProxyWebSocketEcho(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer cannot hijack")
			return
		}
		conn, buf, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		_, _ = fmt.Fprint(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_, _ = io.Copy(conn, buf)
	}))
	defer origin.Close()

	conn, _, cancel, done := startTestStream(t, origin.URL)
	defer cancel()
	defer conn.Close()

	frame := buildConnectRequest("https://demo.trycloudflare.com/ws", connectionTypeWebsocket, []metadata{
		{Key: metadataMethod, Val: "GET"},
		{Key: metadataHost, Val: "demo.trycloudflare.com"},
		{Key: metadataHeader + "Connection", Val: "Upgrade"},
		{Key: metadataHeader + "Upgrade", Val: "websocket"},
	})
	if err := <-writeTestRequest(t, conn, frame); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp := readTestConnectResponse(t, conn)
	if resp.status != http.StatusSwitchingProtocols {
		t.Fatalf("status = %d (err=%q)", resp.status, resp.errText)
	}
	if resp.headers.Get("Upgrade") != "websocket" {
		t.Fatalf("headers = %v", resp.headers)
	}

	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write frame data: %v", err)
	}
	echo := make([]byte, 4)
	if _, err := io.ReadFull(conn, echo); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(echo) != "ping" {
		t.Fatalf("echo = %q", echo)
	}

	_ = conn.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not exit after close")
	}
}

func TestProxyOriginUnreachable(t *testing.T) {
	conn, _, cancel, done := startTestStream(t, "http://127.0.0.1:1")
	defer cancel()
	defer conn.Close()

	frame := buildConnectRequest("https://demo.trycloudflare.com/ws", connectionTypeHTTP, []metadata{
		{Key: metadataMethod, Val: "GET"},
	})
	if err := <-writeTestRequest(t, conn, frame); err != nil {
		t.Fatalf("write request: %v", err)
	}

	resp := readTestConnectResponse(t, conn)
	if resp.status != http.StatusBadGateway || resp.errText == "" {
		t.Fatalf("status=%d error=%q", resp.status, resp.errText)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not exit")
	}
}

// TestStreamReaderCloseCancelsRead 验证请求体 Close 会解除底层读取：
// 源站提前结束响应时，Transport 调用 Close 后不应留下阻塞的读 goroutine。
func TestStreamReaderCloseCancelsRead(t *testing.T) {
	canceled := false
	body := &streamReader{
		r:          strings.NewReader("payload"),
		cancelRead: func() { canceled = true },
	}
	if _, err := io.ReadAll(body); err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !canceled {
		t.Fatal("Close did not cancel the underlying read")
	}
}
