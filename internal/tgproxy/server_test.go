package tgproxy

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

var testSecret = strings.Repeat("ab", 16)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(Config{Secret: testSecret})
	if err != nil {
		t.Fatal(err)
	}
	if s == nil {
		t.Fatal("New returned nil")
	}
	return s
}

func TestNewDisabled(t *testing.T) {
	s, err := New(Config{Secret: ""})
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Fatal("empty secret should disable")
	}
}

func TestNewBadSecret(t *testing.T) {
	if _, err := New(Config{Secret: "not-a-key"}); err == nil {
		t.Fatal("bad secret must fail")
	}
}

func TestGatePageSignature(t *testing.T) {
	s := newTestServer(t)
	host := "example.com"
	sig, err := mkCap(normHex(testSecret), host)
	if err != nil {
		t.Fatal(err)
	}

	// 正确签名 → 返回 bridge 页面
	req := httptest.NewRequest(http.MethodGet, "https://"+host+"/?bridge="+sig, nil)
	req.Host = host
	rec := httptest.NewRecorder()
	if !s.Handle(rec, req) {
		t.Fatal("gate not handled")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<script nonce=") {
		t.Fatal("no bridge script")
	}
	if !strings.Contains(body, "wss://example.com") || !strings.Contains(body, "https://example.com") {
		// origin 以 JSON 字符串形式注入
		if !strings.Contains(body, "https://example.com") {
			t.Fatal("origin not injected")
		}
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "connect-src 'self' wss://example.com") {
		t.Fatalf("csp=%s", csp)
	}

	// 错误签名 → 不处理（回退给主页）
	req2 := httptest.NewRequest(http.MethodGet, "https://"+host+"/?bridge="+strings.Repeat("A", 43), nil)
	req2.Host = host
	rec2 := httptest.NewRecorder()
	if s.Handle(rec2, req2) {
		t.Fatal("bad signature must not be handled")
	}
}

func TestGatePageNonceInCSP(t *testing.T) {
	s := newTestServer(t)
	sig, _ := mkCap(normHex(testSecret), "example.com")
	req := httptest.NewRequest(http.MethodGet, "https://example.com/?bridge="+sig, nil)
	req.Host = "example.com"
	rec := httptest.NewRecorder()
	s.Handle(rec, req)
	nonce := nonceOf(rec.Body.String())
	if len(nonce) < 20 {
		t.Fatalf("nonce=%q", nonce)
	}
	if !strings.Contains(rec.Header().Get("Content-Security-Policy"), "script-src 'nonce-"+nonce+"'") {
		t.Fatal("CSP nonce mismatch")
	}
}

func TestSessionEndpoint(t *testing.T) {
	s := newTestServer(t)
	seed := s.session.seed
	bootstrap := mintToken(seed, "bootstrap", 4102444800000, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	hs := []byte{0x10, 0, 0, 0, 0, 0, 0, 1, 1}

	// POST with handshake + valid bootstrap
	req := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(string(hs)))
	req.Header.Set("Authorization", "Bearer "+bootstrap)
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	s.Handle(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Carrier-Mode") != "websocket-lanes" {
		t.Fatal("missing carrier mode")
	}
	st := rec.Header().Get("X-Session-Token")
	if st == "" || !sigCheck(st, seed, "session") {
		t.Fatal("bad session token")
	}
	// welcome frame 0x11
	if rec.Body.Len() != 8 || rec.Body.Bytes()[0] != frameWelcome {
		t.Fatalf("welcome=%x", rec.Body.Bytes())
	}

	// bad handshake
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader("bad"))
	req2.Header.Set("Authorization", "Bearer "+bootstrap)
	req2.Header.Set("Content-Type", "application/octet-stream")
	rec2 := httptest.NewRecorder()
	s.Handle(rec2, req2)
	if rec2.Code != http.StatusNotFound {
		t.Fatalf("bad hs code=%d", rec2.Code)
	}

	// missing auth
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/session", strings.NewReader(string(hs)))
	req3.Header.Set("Content-Type", "application/octet-stream")
	rec3 := httptest.NewRecorder()
	s.Handle(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("no auth code=%d", rec3.Code)
	}
}

func TestSessionDelete(t *testing.T) {
	s := newTestServer(t)
	seed := s.session.seed
	sess := mintToken(seed, "session", 4102444800000, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/session", nil)
	req.Header.Set("Authorization", "Bearer "+sess)
	rec := httptest.NewRecorder()
	s.Handle(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("code=%d", rec.Code)
	}
}

func TestPathNotHandled(t *testing.T) {
	s := newTestServer(t)
	for _, p := range []string{"/healthz", "/ws", "/api/other"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		if s.Handle(rec, req) {
			t.Fatalf("%s should not be handled", p)
		}
	}
	// 根路径无 bridge 参数不处理
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	if s.Handle(rec, req) {
		t.Fatal("root without bridge should not be handled")
	}
}

// 确保 url.QueryEscape 生成的 search 长度与 gate() 的严格校验一致。
func TestBridgeQueryLen(t *testing.T) {
	u := &url.URL{Path: "/", RawQuery: "bridge=" + strings.Repeat("A", 43)}
	if len(u.RawQuery) != 50 {
		t.Fatalf("rawquery len=%d", len(u.RawQuery))
	}
}
