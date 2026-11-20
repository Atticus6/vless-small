package hy2

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// maxUDPSessions 限制单条 QUIC 连接上的 UDP 会话数，防止客户端无限开 socket。
const maxUDPSessions = 1024

// udpSession 对应一个客户端 UDP 会话（SessionID），持有一个本地 UDP socket。
type udpSession struct {
	id     uint32
	owner  *connHandler
	defrag defragger
	last   atomic.Int64 // UnixNano

	mu   sync.Mutex
	conn *net.UDPConn
	done bool
}

// write 把客户端 payload 发往目标地址；首次使用时创建本地 UDP socket 并
// 启动回包循环。地址来自每条消息，因此同一个会话可以访问多个目标
// （DNS、QUIC 等场景常见）。
func (s *udpSession) write(msg *udpMessage) {
	addr, err := net.ResolveUDPAddr("udp", msg.Addr)
	if err != nil {
		s.owner.srv.cfg.Logf("hy2: udp resolve %q: %v", msg.Addr, err)
		return
	}

	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	if s.conn == nil {
		conn, err := net.ListenUDP("udp", nil)
		if err != nil {
			s.mu.Unlock()
			s.owner.srv.cfg.Logf("hy2: udp listen: %v", err)
			return
		}
		s.conn = conn
		go s.readLoop(conn)
	}
	conn := s.conn
	s.mu.Unlock()

	if _, err := conn.WriteToUDP(msg.Data, addr); err != nil {
		s.owner.srv.cfg.Logf("hy2: udp write %s: %v", addr, err)
	}
}

// readLoop 把目标返回的 UDP 包封装成 Hysteria UDPMessage 发回客户端。
func (s *udpSession) readLoop(conn *net.UDPConn) {
	buf := make([]byte, maxUDPSize)
	for {
		n, raddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			return
		}
		s.last.Store(time.Now().UnixNano())
		out := &udpMessage{
			SessionID: s.id,
			// 未分片的包 FragCount 必须为 1；PacketID/FragID 无意义。
			FragCount: 1,
			Addr:      raddr.String(),
			Data:      append([]byte(nil), buf[:n]...),
		}
		if err := s.owner.sendUDPMessage(out); err != nil {
			return
		}
	}
}

func (s *udpSession) close() {
	s.mu.Lock()
	if s.done {
		s.mu.Unlock()
		return
	}
	s.done = true
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	s.mu.Unlock()
}

// udpSessionManager 管理一条 QUIC 连接上的所有 UDP 会话。
type udpSessionManager struct {
	owner    *connHandler
	idle     time.Duration
	mu       sync.Mutex
	sessions map[uint32]*udpSession
	closed   bool
}

func newUDPSessionManager(owner *connHandler) *udpSessionManager {
	return &udpSessionManager{
		owner:    owner,
		idle:     owner.srv.cfg.UDPIdleTimeout,
		sessions: make(map[uint32]*udpSession),
	}
}

// feed 处理一条来自客户端的 UDP 消息；分片会在会话内重组。
func (m *udpSessionManager) feed(msg *udpMessage) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	session := m.sessions[msg.SessionID]
	if session == nil {
		if len(m.sessions) >= maxUDPSessions {
			m.mu.Unlock()
			return
		}
		session = &udpSession{id: msg.SessionID, owner: m.owner}
		m.sessions[msg.SessionID] = session
	}
	session.last.Store(time.Now().UnixNano())
	m.mu.Unlock()

	full := session.defrag.feed(msg)
	if full == nil {
		return
	}
	session.write(full)
}

// run 定期回收空闲会话，直到 ctx 取消。
func (m *udpSessionManager) run(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			m.closeAll()
			return
		case now := <-ticker.C:
			m.evict(now)
		}
	}
}

func (m *udpSessionManager) evict(now time.Time) {
	deadline := now.Add(-m.idle).UnixNano()
	var expired []*udpSession

	m.mu.Lock()
	for id, session := range m.sessions {
		if session.last.Load() < deadline {
			delete(m.sessions, id)
			expired = append(expired, session)
		}
	}
	m.mu.Unlock()

	for _, session := range expired {
		session.close()
	}
}

func (m *udpSessionManager) closeAll() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	sessions := make([]*udpSession, 0, len(m.sessions))
	for id, session := range m.sessions {
		delete(m.sessions, id)
		sessions = append(sessions, session)
	}
	m.mu.Unlock()

	for _, session := range sessions {
		session.close()
	}
}
