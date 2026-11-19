package quicktunnel

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestLiveTunnelEndToEnd 是可选的真实网络集成测试。
//
// 默认跳过；运行方式：
//
//	LIVE_TUNNEL=1 go test ./internal/quicktunnel -run TestLiveTunnelEndToEnd -v -timeout 240s
//
// 测试会向 api.trycloudflare.com 申请临时隧道，把本地 httptest 服务暴露到
// 公网，并验证 QUIC 注册、本地 ingress 下发、HTTP 与 WebSocket 双向转发。
// 需要节点能出站 UDP 7844；首次访问新域名通常要等待几十秒 DNS 生效。
func TestLiveTunnelEndToEnd(t *testing.T) {
	if os.Getenv("LIVE_TUNNEL") != "1" {
		t.Skip("set LIVE_TUNNEL=1")
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("origin got %s %s host=%s upgrade=%q", r.Method, r.URL.String(), r.Host, r.Header.Get("Upgrade"))
		if r.URL.Path == "/ws" {
			key := r.Header.Get("Sec-WebSocket-Key")
			sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			accept := base64.StdEncoding.EncodeToString(sum[:])
			t.Logf("origin ws key=%q accept=%q", key, accept)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("cannot hijack")
				return
			}
			conn, buf, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)
			_, _ = io.Copy(conn, buf)
			return
		}
		w.Header().Set("X-Origin", "yes")
		_, _ = fmt.Fprintf(w, "hello-live %s", r.URL.Path)
	}))
	defer origin.Close()

	client := New(Config{LocalURL: origin.URL, Version: "live-test", Logf: t.Logf})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var publicURL string
	done := make(chan error, 1)
	go func() {
		done <- client.Run(ctx, func(u string) {
			mu.Lock()
			publicURL = u
			mu.Unlock()
			t.Logf("public URL: %s", u)
		})
	}()

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		u := publicURL
		mu.Unlock()
		if u != "" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if publicURL == "" {
		t.Fatal("no public URL")
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	deadline = time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		resp, err := httpClient.Get(publicURL + "/hello?x=1")
		if err != nil {
			lastErr = err
			time.Sleep(2 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("edge response: %d %q header=%q", resp.StatusCode, string(body), resp.Header.Get("X-Origin"))
		if resp.StatusCode == http.StatusOK && string(body) == "hello-live /hello" && resp.Header.Get("X-Origin") == "yes" {
			testLiveWebSocket(t, publicURL)
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("client did not stop")
			}
			return
		}
		lastErr = fmt.Errorf("status %d body %q", resp.StatusCode, body)
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("tunnel never became reachable: %v", lastErr)
}

func testLiveWebSocket(t *testing.T, publicURL string) {
	t.Helper()
	host := strings.TrimPrefix(publicURL, "https://")
	raw, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", host+":443", &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatalf("ws tls dial: %v", err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(30 * time.Second))
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\n\r\n", host, key)
	if _, err := raw.Write([]byte(req)); err != nil {
		t.Fatalf("ws write handshake: %v", err)
	}
	br := bufio.NewReader(raw)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatalf("ws read response: %v", err)
	}
	t.Logf("ws response: %s upgrade=%q", resp.Status, resp.Header.Get("Upgrade"))
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("ws status = %d", resp.StatusCode)
	}
	if _, err := raw.Write([]byte("ping-ws")); err != nil {
		t.Fatalf("ws write data: %v", err)
	}
	echo := make([]byte, len("ping-ws"))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("ws read echo: %v", err)
	}
	if string(echo) != "ping-ws" {
		t.Fatalf("ws echo = %q", echo)
	}
	t.Logf("ws echo ok: %q", echo)
}
