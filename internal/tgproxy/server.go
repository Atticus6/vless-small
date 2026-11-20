package tgproxy

import (
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

//go:embed bridge.html.tpl
var bridgeTemplate string

// Config 是 TG web-proxy 服务端的配置。
type Config struct {
	// Secret 是节点 SECRET（32 hex 或 dd/ee/ef 前缀 + 32 hex，也接受 UUID 形式）。
	// 为空时功能关闭。
	Secret string
	// Logf 可选日志函数。
	Logf func(string, ...any)
}

// Server 是 tgproxy 的 HTTP 服务端；把它挂到 vless-small 的 HTTP handler 上。
type Server struct {
	cfg      Config
	session  *session
	upgrader websocket.Upgrader
}

// New 创建服务端；secret 非法时返回错误。
func New(cfg Config) (*Server, error) {
	if strings.TrimSpace(cfg.Secret) == "" {
		return nil, nil // 未配置 → 不启用
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	skey := normHex(cfg.Secret)
	key, err := keyFrom(skey, false)
	if err != nil {
		return nil, err
	}
	seed, err := deriveSeed(skey)
	if err != nil {
		return nil, err
	}
	return &Server{
		cfg:     cfg,
		session: newSession(seed, key),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  32 * 1024,
			WriteBufferSize: 32 * 1024,
			CheckOrigin:     func(*http.Request) bool { return true }, // 已在 laneAPI 里按 host 校验
		},
	}, nil
}

// BridgeURL 生成给 Telegram Desktop 用的 ?bridge= 页面 URL。
//
// host 是节点对外的 HTTPS 域名（不含协议），签名只对这个 host 有效。
func (s *Server) BridgeURL(host string) (string, error) {
	if s == nil {
		return "", errors.New("tgproxy disabled")
	}
	h := validHost(host)
	if h == "" {
		return "", errors.New("invalid host")
	}
	sig, err := mkCap(normHex(s.cfg.Secret), h)
	if err != nil {
		return "", err
	}
	return "https://" + h + "/?bridge=" + sig, nil
}

// Handle 尝试处理 tgproxy 路由；返回 true 表示已处理，false 表示不属于本服务。
func (s *Server) Handle(w http.ResponseWriter, r *http.Request) bool {
	if s == nil {
		return false
	}
	path := r.URL.Path
	switch path {
	case "/api/v1/session":
		s.session.sessAPI(w, r)
		return true
	case "/api/v1/ws":
		s.session.laneAPI(w, r, s.upgrader)
		return true
	}
	if path == "/" && r.Method == http.MethodGet {
		if s.gatePage(w, r) {
			return true
		}
	}
	return false
}

// gatePage 处理 /?bridge=<sig>，签名通过返回 carrier 页面。
func (s *Server) gatePage(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	if !q.Has("bridge") {
		return false
	}
	sig := q.Get("bridge")
	// 与 JS gate() 一致：search 必须恰好是 ?bridge=<43 字符>
	if len(r.URL.RawQuery) != 7+43 || !strings.HasPrefix(r.URL.RawQuery, "bridge=") ||
		len(sig) != 43 {
		return false
	}
	for _, c := range sig {
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	host := validHost(r.Host)
	if host == "" {
		return false
	}
	want, err := mkCap(normHex(s.cfg.Secret), host)
	if err != nil || want != sig {
		return false
	}

	body := renderBridge(bridgeTemplate, "https://"+host, s.session.seed)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", bridgeCSP(host, nonceOf(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
	return true
}

// renderBridge 用 origin/seed 替换模板占位符，nonce 用随机 24 字符。
func renderBridge(tpl, origin, seed string) string {
	nonce := b64url(randNonce(18))
	body := strings.ReplaceAll(tpl, "{{NONCE}}", nonce)
	body = strings.Replace(body, "{{ORIGIN}}", jsonQuote(origin), 1)
	body = strings.Replace(body, "{{BOOTSTRAP}}", jsonQuote(seed), 1)
	return body
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// nonceOf 从渲染后的 <script nonce="..."> 中提取 nonce。
func nonceOf(body string) string {
	const prefix = `<script nonce="`
	i := strings.Index(body, prefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(prefix):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return rest[:j]
}

func bridgeCSP(host, nonce string) string {
	parts := []string{
		"default-src 'none'", "base-uri 'none'", "child-src 'none'",
		"connect-src 'self' wss://" + host,
		"font-src 'none'", "form-action 'none'",
		"frame-ancestors http://127.0.0.1:*", "frame-src 'none'",
		"img-src 'none'", "manifest-src 'none'", "media-src 'none'",
		"object-src 'none'", "script-src 'nonce-" + nonce + "'",
		"style-src 'none'", "worker-src 'none'",
		"sandbox allow-same-origin allow-scripts",
	}
	return strings.Join(parts, "; ")
}
