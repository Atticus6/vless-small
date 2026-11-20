package hy2

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/apernet/quic-go/quicvarint"
)

func TestReadTCPRequest(t *testing.T) {
	const addr = "example.com:443"
	var buf []byte
	buf = quicvarint.Append(buf, uint64(len(addr)))
	buf = append(buf, addr...)
	buf = quicvarint.Append(buf, 4)
	buf = append(buf, "pad!"...)

	got, err := readTCPRequest(bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("readTCPRequest: %v", err)
	}
	if got != addr {
		t.Fatalf("addr = %q, want %q", got, addr)
	}
}

func TestReadTCPRequestRejectsInvalid(t *testing.T) {
	tooLongAddr := quicvarint.Append(nil, maxAddressLength+1)
	tooLongPadding := append(
		quicvarint.Append(nil, 3), "a:1"...,
	)
	tooLongPadding = quicvarint.Append(tooLongPadding, maxPaddingLength+1)

	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "zero address", data: quicvarint.Append(nil, 0)},
		{name: "address too long", data: tooLongAddr},
		{name: "truncated address", data: append(quicvarint.Append(nil, 5), 'a', 'b')},
		{name: "padding too long", data: tooLongPadding},
	}

	for _, tt := range tests {
		if _, err := readTCPRequest(bytes.NewReader(tt.data)); err == nil {
			t.Fatalf("%s: expected error", tt.name)
		}
	}
}

func TestWriteTCPResponse(t *testing.T) {
	const message = "Connected"
	var buf bytes.Buffer
	if err := writeTCPResponse(&buf, true, message); err != nil {
		t.Fatalf("writeTCPResponse: %v", err)
	}

	data := buf.Bytes()
	if len(data) == 0 || data[0] != 0 {
		t.Fatalf("status = %v, want OK", data)
	}
	reader := bytes.NewReader(data[1:])
	br := quicvarint.NewReader(reader)
	length, err := quicvarint.Read(br)
	if err != nil {
		t.Fatalf("read message length: %v", err)
	}
	got := make([]byte, length)
	if _, err := io.ReadFull(reader, got); err != nil {
		t.Fatalf("read message: %v", err)
	}
	if string(got) != message {
		t.Fatalf("message = %q, want %q", got, message)
	}
	padding, err := quicvarint.Read(br)
	if err != nil {
		t.Fatalf("read padding length: %v", err)
	}
	if padding != 0 {
		t.Fatalf("padding length = %d, want 0", padding)
	}
}

func TestUDPMessageRoundTrip(t *testing.T) {
	want := &udpMessage{
		SessionID: 0x01020304,
		PacketID:  0x0506,
		FragID:    1,
		FragCount: 3,
		Addr:      "[2001:db8::1]:5353",
		Data:      []byte("payload"),
	}

	got, err := parseUDPMessage(want.appendTo(nil))
	if err != nil {
		t.Fatalf("parseUDPMessage: %v", err)
	}
	if got.SessionID != want.SessionID || got.PacketID != want.PacketID ||
		got.FragID != want.FragID || got.FragCount != want.FragCount ||
		got.Addr != want.Addr || !bytes.Equal(got.Data, want.Data) {
		t.Fatalf("message = %+v, want %+v", got, want)
	}
}

func TestParseUDPMessageRejectsInvalid(t *testing.T) {
	valid := (&udpMessage{Addr: "a:1", Data: []byte("x")}).appendTo(nil)
	tests := []struct {
		name string
		data []byte
	}{
		{name: "short", data: []byte{0, 1, 2}},
		{name: "zero address", data: append(valid[:8], quicvarint.Append(nil, 0)...)},
		{name: "truncated address", data: append(valid[:8], quicvarint.Append(nil, 8)...)},
	}

	for _, tt := range tests {
		if _, err := parseUDPMessage(tt.data); err == nil {
			t.Fatalf("%s: expected error", tt.name)
		}
	}
}

func TestFragmentAndDefrag(t *testing.T) {
	want := &udpMessage{
		SessionID: 7,
		PacketID:  42,
		Addr:      "1.1.1.1:53",
		Data:      bytes.Repeat([]byte("x"), 4000),
	}
	frags := fragmentUDPMessage(want, 1200)
	if len(frags) < 2 {
		t.Fatalf("fragments = %d, want >= 2", len(frags))
	}

	var d defragger
	var assembled *udpMessage
	for i, frag := range frags {
		assembled = d.feed(frag)
		if i < len(frags)-1 && assembled != nil {
			t.Fatalf("assembled before last fragment (i=%d)", i)
		}
	}
	if assembled == nil {
		t.Fatal("message was not assembled")
	}
	if !bytes.Equal(assembled.Data, want.Data) {
		t.Fatalf("assembled data length = %d, want %d", len(assembled.Data), len(want.Data))
	}
	if assembled.FragCount != 1 || assembled.FragID != 0 {
		t.Fatalf("assembled header = frag %d/%d, want 0/1", assembled.FragID, assembled.FragCount)
	}
}

func TestRandomPadding(t *testing.T) {
	for _, n := range []int{0, 1, 16, 64} {
		if got := randomPadding(n); len(got) != n {
			t.Fatalf("randomPadding(%d) length = %d", n, len(got))
		}
	}
}

func TestAuthRejectsWrongPassword(t *testing.T) {
	h := &connHandler{srv: &Server{cfg: Config{Password: "secret"}}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://hysteria/auth", strings.NewReader(""))
	req.Host = authHost
	req.Header.Set(headerAuth, "wrong")

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if h.authenticated.Load() {
		t.Fatal("wrong password authenticated the connection")
	}
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("missing listen should fail")
	}
	// 既没有共享密码也没有认证回调时无法完成认证，必须拒绝启动。
	if _, err := New(Config{Listen: "127.0.0.1:0"}); err == nil {
		t.Fatal("missing password and authenticate callback should fail")
	}
	// 只给认证回调（接主控时的形态）是合法配置。
	withCallback, err := New(Config{
		Listen:       "127.0.0.1:0",
		Authenticate: func(string) bool { return true },
		CertDir:      t.TempDir(),
	})
	if err != nil {
		t.Fatalf("authenticate-only config should work: %v", err)
	}
	defer withCallback.Close()

	// 不传证书是合法配置：会自动生成自签证书。
	srv, err := New(Config{
		Listen:   "127.0.0.1:0",
		Password: "x",
		CertDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("self-signed config should work: %v", err)
	}
	defer srv.Close()
}

// TestAuthenticateCallback 验证认证回调可以替代共享密码：回调返回 true 时
// 通过、返回 false 时仍然 404，回调收到的是客户端提交的原始字符串。
func TestAuthenticateCallback(t *testing.T) {
	const uuid = "11111111-1111-4111-8111-111111111111"
	var seen []string
	srv := &Server{cfg: Config{
		Authenticate: func(auth string) bool {
			seen = append(seen, auth)
			return auth == uuid
		},
	}}

	ok := &connHandler{srv: srv}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "https://hysteria/auth", strings.NewReader(""))
	req.Host = authHost
	req.Header.Set(headerAuth, uuid)
	ok.ServeHTTP(rec, req)

	if rec.Code != statusAuthOK {
		t.Fatalf("status = %d, want %d", rec.Code, statusAuthOK)
	}
	if !ok.authenticated.Load() {
		t.Fatal("callback accepted the uuid but connection is not authenticated")
	}

	bad := &connHandler{srv: srv}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "https://hysteria/auth", strings.NewReader(""))
	req.Host = authHost
	req.Header.Set(headerAuth, "not-a-uuid")
	bad.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if bad.authenticated.Load() {
		t.Fatal("callback rejected the value but connection is authenticated")
	}
	if len(seen) != 2 || seen[1] != "not-a-uuid" {
		t.Fatalf("callback saw %v, want the raw header value", seen)
	}
}

// TestAuthenticateRejectsEmpty 验证空认证头不会触发回调，避免回调把空串
// 当成合法输入（例如把空串解析成全零 UUID）。
func TestAuthenticateRejectsEmpty(t *testing.T) {
	called := false
	srv := &Server{cfg: Config{
		Authenticate: func(string) bool {
			called = true
			return true
		},
	}}

	if srv.authenticate("") {
		t.Fatal("empty auth should never pass")
	}
	if called {
		t.Fatal("callback should not be called for empty auth")
	}
}

// TestStatusEnabled 验证成功绑定端口后上报 enabled=true。
//
// 端口/SNI/证书类型都是固定约定，主控不依赖节点上报，因此这里只校验
// 唯一的字段。
func TestStatusEnabled(t *testing.T) {
	srv, err := New(Config{
		Listen:   "127.0.0.1:0",
		Password: "secret",
		CertDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()

	if !srv.Status().Enabled {
		t.Fatal("server bound the port but status is not enabled")
	}
}

// TestSelfSignedCertificateCached 验证自签证书会生成到磁盘并在第二次加载时
// 复用，避免每次重启都重新生成。
func TestSelfSignedCertificateCached(t *testing.T) {
	dir := t.TempDir()

	first, err := loadOrCreateSelfSigned(dir, "bing.com")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if len(first.Certificate) == 0 || first.PrivateKey == nil {
		t.Fatal("generated certificate is incomplete")
	}
	if first.Leaf == nil {
		t.Fatal("generated certificate has no parsed leaf")
	}
	if first.Leaf.Subject.CommonName != "bing.com" {
		t.Fatalf("CN = %q, want bing.com", first.Leaf.Subject.CommonName)
	}

	// 再次加载应命中缓存：公钥相同。
	second, err := loadOrCreateSelfSigned(dir, "bing.com")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !first.Leaf.Equal(second.Leaf) {
		t.Fatal("certificate was regenerated instead of reused")
	}

	// 换域名时必须重新生成，否则 SNI 与证书不匹配。
	third, err := loadOrCreateSelfSigned(dir, "example.com")
	if err != nil {
		t.Fatalf("regenerate: %v", err)
	}
	if third.Leaf.Subject.CommonName != "example.com" {
		t.Fatalf("CN = %q, want example.com", third.Leaf.Subject.CommonName)
	}
}

// TestSelfSignedCertificateDefaultDomain 验证未指定域名时使用 bing.com。
func TestSelfSignedCertificateDefaultDomain(t *testing.T) {
	cert, err := loadOrCreateSelfSigned(t.TempDir(), "")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if cert.Leaf.Subject.CommonName != defaultSelfSignedDomain {
		t.Fatalf("CN = %q, want %q", cert.Leaf.Subject.CommonName, defaultSelfSignedDomain)
	}
}

// TestNewUsesSelfSignedWhenNoCert 验证不传证书也能启动，并使用自签证书。
func TestNewUsesSelfSignedWhenNoCert(t *testing.T) {
	srv, err := New(Config{
		Listen:   "127.0.0.1:0",
		Password: "secret",
		CertDir:  t.TempDir(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer srv.Close()
	if srv.Addr() == nil {
		t.Fatal("server did not bind")
	}
}

// TestNewRejectsHalfCertificate 验证只给证书或只给私钥时报错。
func TestNewRejectsHalfCertificate(t *testing.T) {
	if _, err := New(Config{
		Listen:   "127.0.0.1:0",
		Password: "secret",
		CertFile: "cert.pem",
		CertDir:  t.TempDir(),
	}); err == nil {
		t.Fatal("cert without key should fail")
	}
}
