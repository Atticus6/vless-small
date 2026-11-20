package tgproxy

import (
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// 全局 lease 表：session token → 租约（与 JS 的 leases Map 对应）。
type lease struct {
	expires int64
	active  map[uint32]bool
	used    map[uint32]bool
	deleted bool
}
type session struct {
	mu     sync.Mutex
	seed   string
	cfgKey []byte // skey 对应的 AES key，给 lane 的 stream 用
	leases map[string]*lease
}

func newSession(seed string, cfgKey []byte) *session {
	return &session{seed: seed, cfgKey: cfgKey, leases: make(map[string]*lease)}
}

// sessAPI 处理 POST/DELETE /api/v1/session。
func (s *session) sessAPI(w http.ResponseWriter, r *http.Request) {
	tok := tokenFrom(r.Header.Get("Authorization"))
	switch r.Method {
	case http.MethodDelete:
		if tok == "" || !sigCheck(tok, s.seed, "session") {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		l := s.leases[tok]
		if l == nil {
			l = &lease{active: map[uint32]bool{}, used: map[uint32]bool{}}
			s.leases[tok] = l
		}
		l.deleted = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return

	case http.MethodPost:
		if tok == "" || r.Header.Get("Content-Type") != "application/octet-stream" ||
			!checkSeed(tok, s.seed) {
			http.NotFound(w, r)
			return
		}
		defer r.Body.Close()
		body := make([]byte, 10)
		n, _ := r.Body.Read(body)
		if !checkHandshake(body[:n]) {
			http.NotFound(w, r)
			return
		}
		sessTok := mintToken(s.seed, "session", time.Now().Add(5*time.Minute).UnixMilli(), randNonce(12))
		welcome, _ := encodeFrame(frameWelcome, 0, nil)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Session-Token", sessTok)
		w.Header().Set("X-Carrier-Mode", "websocket-lanes")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(welcome)

	default:
		http.NotFound(w, r)
	}
}

// laneAPI 处理 GET /api/v1/ws 升级，一条连接 = 一条 lane。
func (s *session) laneAPI(w http.ResponseWriter, r *http.Request, upgrader websocket.Upgrader) {
	if r.Method != http.MethodGet || r.Header.Get("Upgrade") != "websocket" {
		http.NotFound(w, r)
		return
	}
	// Origin 校验（若浏览器带了 Origin 必须与本机同源）。
	if origin := r.Header.Get("Origin"); origin != "" {
		if o, err := parseOrigin(origin); err != nil || o != r.Host {
			http.NotFound(w, r)
			return
		}
	}
	offers := r.Header.Get("Sec-WebSocket-Protocol")
	if offers == "" {
		http.NotFound(w, r)
		return
	}
	// proto 形如 tproxy-lane-v1.<token>.<laneId>
	const kind = "tpro" + "xy-lane-v1"
	var proto, tok, idStr string
	if parts := splitProto(offers); len(parts) == 3 && parts[0] == kind {
		proto, tok, idStr = parts[0], parts[1], parts[2]
	} else {
		http.NotFound(w, r)
		return
	}
	laneID := parseLaneID(idStr)
	if laneID == 0 || laneID > maxSID || !sigCheck(tok, s.seed, "session") {
		http.NotFound(w, r)
		return
	}

	// 租约检查。
	s.mu.Lock()
	now := time.Now().UnixMilli()
	for k, v := range s.leases {
		if v.expires <= now && len(v.active) == 0 {
			delete(s.leases, k)
		}
	}
	l := s.leases[tok]
	if l == nil {
		if len(s.leases) >= 4096 {
			s.mu.Unlock()
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		l = &lease{expires: time.Now().Add(5 * time.Minute).UnixMilli(), active: map[uint32]bool{}, used: map[uint32]bool{}}
		s.leases[tok] = l
	}
	if l.deleted || l.used[laneID] || len(l.active) >= 128 || len(l.used) >= 4096 {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	l.used[laneID] = true
	l.active[laneID] = true
	s.mu.Unlock()

	ws, err := upgrader.Upgrade(w, r, http.Header{"Sec-WebSocket-Protocol": []string{proto}, "Sec-WebSocket-Extensions": []string{""}})
	if err != nil {
		s.mu.Lock()
		delete(l.active, laneID)
		s.mu.Unlock()
		return
	}
	ln := newLane(s.cfgKey, laneID, ws, func() {
		s.mu.Lock()
		delete(l.active, laneID)
		s.mu.Unlock()
	})
	ln.run()
}

func splitProto(v string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(v); i++ {
		if i == len(v) || v[i] == ',' {
			out = append(out, trimSpace(v[start:i]))
			start = i + 1
		}
	}
	return out
}

func parseLaneID(s string) uint32 {
	if len(s) == 0 || len(s) > 8 || s[0] == '0' {
		return 0
	}
	var n uint32
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + uint32(c-'0')
	}
	return n
}

func parseOrigin(origin string) (string, error) {
	// 简易解析 http(s)://host[:port]
	if len(origin) < 8 {
		return "", errors.New("bad origin")
	}
	scheme := "http://"
	if len(origin) >= 9 && origin[:9] == "https://" {
		scheme = "https://"
	}
	rest := origin[len(scheme):]
	for i, c := range rest {
		if c == '/' {
			rest = rest[:i]
			break
		}
	}
	return rest, nil
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\t') {
		s = s[1:]
	}
	for len(s) > 0 && (s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func randNonce(n int) []byte {
	b := make([]byte, n)
	readRand(b)
	return b
}

// lane 对应 JS 的 makeSess：把 lane WebSocket 的帧事件转成 tgStream 操作。
type lane struct {
	key     []byte
	sid     uint32
	ws      *websocket.Conn
	stream  *tgStream
	onClose func()

	writeMu sync.Mutex
	closed  bool
}

func newLane(key []byte, sid uint32, ws *websocket.Conn, onClose func()) *lane {
	ln := &lane{key: key, sid: sid, ws: ws, onClose: onClose}
	ln.stream = newTGStream(key, ln.emitDown, func() { ln.sendCloseNotify() })
	return ln
}

// emitDown 是 tgStream 的下行回调：把 TG 数据封成 data 帧写回 lane。
func (l *lane) emitDown(b []byte) error {
	frame, err := encodeFrame(frameData, l.sid, b)
	if err != nil {
		return err
	}
	return l.sendFrame(frame)
}

// sendCloseNotify 在 TG 流结束时通知客户端关闭该 lane。
func (l *lane) sendCloseNotify() {
	frame, _ := encodeFrame(frameClose, l.sid, nil)
	_ = l.sendFrame(frame)
}

func (l *lane) sendFrame(b []byte) error {
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	if l.closed {
		return errors.New("closed")
	}
	return l.ws.WriteMessage(websocket.BinaryMessage, b)
}

// run 驱动 lane 生命周期：读 WebSocket 消息 → 按帧分发。
func (l *lane) run() {
	defer l.close()
	for {
		_, msg, err := l.ws.ReadMessage()
		if err != nil {
			return
		}
		if len(msg) == 0 || len(msg) > 2*1024*1024 {
			return
		}
		frames, err := splitFrames(msg)
		if err != nil {
			return
		}
		for _, f := range frames {
			if f.SID != l.sid {
				return
			}
			switch f.Type {
			case frameOpen:
				if l.stream.init || l.stream.closed {
					return
				}
			case frameData:
				if err := l.stream.push(f.Payload); err != nil {
					return
				}
			case frameWindow:
				if _, err := windowDelta(f.Payload); err != nil {
					return
				}
				// window 流控：Go 端用无界缓冲简化实现，收到 window 只需校验格式。
			case frameClose:
				l.stream.close()
				return
			default:
				return
			}
		}
	}
}

func (l *lane) close() {
	l.writeMu.Lock()
	if l.closed {
		l.writeMu.Unlock()
		return
	}
	l.closed = true
	l.writeMu.Unlock()
	l.stream.close()
	_ = l.ws.Close()
	if l.onClose != nil {
		l.onClose()
	}
}
