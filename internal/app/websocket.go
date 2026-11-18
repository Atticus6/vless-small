package app

import (
	"encoding/binary"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// handleWS 完成握手、认证, 然后按命令分发到 TCP/UDP 转发.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.active.Add(1)
	defer s.active.Add(-1)

	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		if s.handleHealth(w, r) {
			return
		}
		http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
		return
	}
	if s.path != "" && r.URL.Path != s.path {
		http.NotFound(w, r)
		return
	}

	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("upgrade %s: %v", r.RemoteAddr, err)
		return
	}
	defer ws.Close()

	ws.SetReadLimit(maxMessageSize)
	// 认证完成前不续期, 防止未认证连接靠 pong 一直占着资源.
	ws.SetReadDeadline(time.Now().Add(headerTimeout))

	// 首条 WS 消息固定是 VLESS 请求头, 可能附带 early data.
	_, head, err := ws.ReadMessage()
	if err != nil {
		log.Printf("read header %s: %v", r.RemoteAddr, err)
		return
	}
	id, cmd, target, payload, err := parseRequest(head)
	if err != nil {
		log.Printf("bad request %s: %v", r.RemoteAddr, err)
		return
	}
	if !s.hasUser(id) {
		log.Printf("reject %s: unknown UUID", r.RemoteAddr)
		return
	}
	s.addActiveUUID(id)
	// 连接在 relay 返回后关闭；先登记再 defer，确保异常退出也会清理。
	defer s.removeActiveUUID(id)

	// 认证通过后再启用 pong 续期, 未认证连接无法拖延 headerTimeout.
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(readTimeout))
	})
	ws.SetReadDeadline(time.Now().Add(readTimeout))

	// 保活: 定时 ping, 客户端回 pong 时续期读超时.
	stopPing := make(chan struct{})
	defer close(stopPing)
	go pingLoop(ws, stopPing)

	log.Printf("open %s (cmd=%d)", r.RemoteAddr, cmd)
	switch cmd {
	case cmdTCP:
		s.relayTCP(ws, target, payload)
	case cmdUDP:
		s.relayUDP(ws, target, payload)
	default:
		log.Printf("unsupported command %d from %s", cmd, r.RemoteAddr)
	}
}

// pingLoop 定期发送控制帧; WriteControl 可与数据写并发调用, 无需额外加锁.
func pingLoop(ws *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second)); err != nil {
				ws.Close()
				return
			}
		}
	}
}

// relayTCP 双向转发: WebSocket <-> 目标 TCP. 仅本 goroutine 写 WS 数据帧.
func (s *Server) relayTCP(ws *websocket.Conn, target string, early []byte) {
	conn, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		log.Printf("dial %s: %v", target, err)
		return
	}
	defer conn.Close()

	// VLESS 响应头: version + addon 长度 0.
	if err := writeMessage(ws, []byte{vlessVersion, 0}); err != nil {
		return
	}
	if len(early) > 0 {
		_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
		if _, err := conn.Write(early); err != nil {
			log.Printf("write early data %s: %v", target, err)
			return
		}
	}

	// WS -> 目标; 出错时关 conn, 让下面的读循环退出.
	go func() {
		defer conn.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
			if len(data) == 0 {
				continue
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := conn.Write(data); err != nil {
				return
			}
		}
	}()

	// 目标 -> WS.
	buf := make([]byte, bufferSize)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			if err := writeMessage(ws, buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// relayUDP 转发 VLESS UDP 会话.
// 包格式: [2B 大端长度][payload], 一个 WS 消息里可以拼接多个包.
func (s *Server) relayUDP(ws *websocket.Conn, target string, early []byte) {
	conn, err := net.DialTimeout("udp", target, dialTimeout)
	if err != nil {
		log.Printf("dial udp %s: %v", target, err)
		return
	}
	defer conn.Close()

	if err := writeMessage(ws, []byte{vlessVersion, 0}); err != nil {
		return
	}
	framer := &udpFramer{}
	if len(early) > 0 {
		if err := framer.feed(conn, early); err != nil {
			return
		}
	}

	go func() {
		defer conn.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
			if len(data) > 0 {
				if err := framer.feed(conn, data); err != nil {
					return
				}
			}
		}
	}()

	// 回包: 每个 datagram 前加 2 字节长度, 再作为一条 WS 消息发出.
	buf := make([]byte, 64*1024+2)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		n, err := conn.Read(buf[2:])
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		binary.BigEndian.PutUint16(buf[:2], uint16(n))
		if err := writeMessage(ws, buf[:2+n]); err != nil {
			return
		}
	}
}

// udpFramer 按 VLESS UDP 字节流重组 [2B 长度][payload] 包.
// 包边界与 WebSocket 消息边界无关, 不完整的包留到下一帧继续拼.
type udpFramer struct {
	buf []byte
}

func (f *udpFramer) feed(conn net.Conn, data []byte) error {
	f.buf = append(f.buf, data...)
	for {
		if len(f.buf) < 2 {
			return nil
		}
		n := int(binary.BigEndian.Uint16(f.buf[:2]))
		if len(f.buf) < 2+n {
			return nil
		}
		if n > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := conn.Write(f.buf[2 : 2+n]); err != nil {
				return err
			}
			// 上行数据也算会话活跃, 避免只发不收时被 idle 超时误杀.
			_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		}
		f.buf = f.buf[2+n:]
	}
}

// writeMessage 写一条二进制 WS 消息; 调用方需保证只有单个 goroutine 写数据帧.
func writeMessage(ws *websocket.Conn, data []byte) error {
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return ws.WriteMessage(websocket.BinaryMessage, data)
}
