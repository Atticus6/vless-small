package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// server 持有可热更新的用户表与 WS 配置, 可被多个连接并发使用.
type server struct {
	users     atomic.Pointer[map[[16]byte]struct{}]
	path      string // WebSocket 路径；为空表示任意路径都接受
	xhttpPath string // XHTTP stream-one 路径；为空表示不启用
	upgrader  websocket.Upgrader
	active    atomic.Int64
	startedAt time.Time
}

type healthResponse struct {
	OK            bool   `json:"ok"`
	Uptime        string `json:"uptime"`
	UptimeSeconds int64  `json:"uptimeSeconds"`
}

func newServer(users map[[16]byte]struct{}, path string) *server {
	s := &server{
		path:      path,
		startedAt: time.Now(),
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(*http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
	}
	s.setUsers(users)
	return s
}

func (s *server) setUsers(users map[[16]byte]struct{}) {
	if users == nil {
		users = map[[16]byte]struct{}{}
	}
	s.users.Store(&users)
}

func (s *server) hasUser(id [16]byte) bool {
	users := s.users.Load()
	if users == nil {
		return false
	}
	_, ok := (*users)[id]
	return ok
}

func (s *server) userCount() int {
	users := s.users.Load()
	if users == nil {
		return 0
	}
	return len(*users)
}

// handleHTTP 按路径分发 WebSocket / XHTTP stream-one 请求.
func (s *server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if s.xhttpPath != "" && strings.HasPrefix(r.URL.Path, s.xhttpPath) {
		s.handleXHTTP(w, r)
		return
	}
	s.handleWS(w, r)
}

// handleHealth 处理普通 HTTP 探活请求.
//
// 浏览器/探活请求走普通 HTTP：根路径和 /healthz 返回健康信息，
// 避免直接看到 426/400；真正的 WebSocket 路径仍要求 Upgrade。
// 返回 false 表示该请求不是健康检查，由调用方继续返回 426.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}

	switch r.URL.Path {
	case "/healthz":
		uptime := time.Since(s.startedAt)

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(healthResponse{
			OK:            true,
			Uptime:        uptime.Truncate(time.Second).String(),
			UptimeSeconds: int64(uptime.Seconds()),
		})
		return true
	case "/":
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("vless-small ok\n"))
		return true
	default:
		return false
	}
}

// waitDrain 等待已升级的 WebSocket 会话结束, 最多等 timeout.
func (s *server) waitDrain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for s.active.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}
