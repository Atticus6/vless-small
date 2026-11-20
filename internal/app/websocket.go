package app

import (
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
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
	sourceIP, ok := s.sourceIPForPath(r.URL.Path)
	if !ok {
		// 形如 /ipN 但不在当前出口列表里：明确 404，避免静默使用默认出口。
		http.NotFound(w, r)
		return
	}
	if s.path != "" && r.URL.Path != s.path && sourceIP == nil {
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

	egress := "-"
	if sourceIP != nil {
		egress = sourceIP.String()
	}
	log.Printf("open %s (cmd=%d, path=%s, egress=%s)", r.RemoteAddr, cmd, r.URL.Path, egress)
	switch cmd {
	case cmdTCP:
		s.relayTCP(ws, target, payload, sourceIP, id)
	case cmdUDP:
		s.relayUDP(ws, target, payload, sourceIP, id)
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

// relayBufferPool 复用 TCP 转发缓冲区。
//
// 大流量下每条连接每秒会经历成百上千次读；复用固定大小的缓冲区可以避免
// 每条连接各自持有一份，也给 GC 减压。
var relayBufferPool = sync.Pool{
	New: func() any { return make([]byte, bufferSize) },
}

// udpRelayBufferPool 复用 UDP 回包缓冲区（最大 datagram + 2 字节长度头）。
var udpRelayBufferPool = sync.Pool{
	New: func() any { return make([]byte, 64*1024+2) },
}

// relayTCP 双向转发: WebSocket <-> 目标 TCP. 仅本 goroutine 写 WS 数据帧.
// sourceIP 非空时绑定该本地地址作为出站源 IP；userID 用于累计流量。
func (s *Server) relayTCP(
	ws *websocket.Conn,
	target string,
	early []byte,
	sourceIP net.IP,
	userID [16]byte,
) {
	conn, err := dialOutbound(context.Background(), "tcp", target, sourceIP)
	if err != nil {
		log.Printf("dial %s from %s: %v", target, sourceIP, err)
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
		s.addTraffic(userID, uint64(len(early)), 0)
	}

	// WS -> 目标。用 NextReader 流式读取消息内容，并先把不足一个缓冲区的
	// 分片合并再写目标连接：一条大消息可能被拆成多个 WS 帧（gorilla 默认
	// 4KB 一帧），逐帧 Write 会把一次 32KB 写放大成八次系统调用。用池化
	// 缓冲区合并后，既保留“不为每条消息分配新内存”的优点，又不会牺牲
	// 大块写的吞吐。
	go func() {
		defer conn.Close()
		buf := relayBufferPool.Get().([]byte)
		defer relayBufferPool.Put(buf)

		for {
			_, reader, err := ws.NextReader()
			if err != nil {
				return
			}

			filled := 0
			for {
				n, readErr := reader.Read(buf[filled:])
				filled += n
				if filled == len(buf) {
					if !writeUpstream(conn, buf[:filled], s, userID) {
						return
					}
					filled = 0
				}
				// io.EOF 表示当前消息读完，继续处理下一条；其他错误
				// （连接中断、超时、协议错误）直接结束会话。
				if readErr != nil {
					if readErr == io.EOF {
						break
					}
					return
				}
			}
			if filled > 0 && !writeUpstream(conn, buf[:filled], s, userID) {
				return
			}
			_ = ws.SetReadDeadline(time.Now().Add(readTimeout))
		}
	}()

	// 目标 -> WS.
	buf := relayBufferPool.Get().([]byte)
	defer relayBufferPool.Put(buf)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			s.addTraffic(userID, 0, uint64(n))
			if err := writeMessage(ws, buf[:n]); err != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// writeUpstream 把一段上行数据写入目标连接并累计流量.
// 返回 false 表示写入失败，调用方应结束会话。
func writeUpstream(conn net.Conn, data []byte, s *Server, userID [16]byte) bool {
	_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	written, err := conn.Write(data)
	if written > 0 {
		s.addTraffic(userID, uint64(written), 0)
	}
	return err == nil
}

// relayUDP 转发 VLESS UDP 会话.
// 包格式: [2B 大端长度][payload], 一个 WS 消息里可以拼接多个包.
// sourceIP 非空时绑定该本地地址作为出站源 IP；userID 用于累计流量。
func (s *Server) relayUDP(
	ws *websocket.Conn,
	target string,
	early []byte,
	sourceIP net.IP,
	userID [16]byte,
) {
	conn, err := dialOutbound(context.Background(), "udp", target, sourceIP)
	if err != nil {
		log.Printf("dial udp %s from %s: %v", target, sourceIP, err)
		return
	}
	defer conn.Close()

	if err := writeMessage(ws, []byte{vlessVersion, 0}); err != nil {
		return
	}
	framer := &udpFramer{}
	if len(early) > 0 {
		written, err := framer.feed(conn, early)
		if written > 0 {
			s.addTraffic(userID, uint64(written), 0)
		}
		if err != nil {
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
				written, err := framer.feed(conn, data)
				if written > 0 {
					s.addTraffic(userID, uint64(written), 0)
				}
				if err != nil {
					return
				}
			}
		}
	}()

	// 回包: 每个 datagram 前加 2 字节长度, 再作为一条 WS 消息发出.
	// 缓冲区从池里取：DNS/QUIC 场景下 UDP 会话建立频繁，避免每次分配 64KB。
	buf := udpRelayBufferPool.Get().([]byte)
	defer udpRelayBufferPool.Put(buf)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		n, err := conn.Read(buf[2:])
		if err != nil {
			return
		}
		if n == 0 {
			continue
		}
		s.addTraffic(userID, 0, uint64(n))
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

// feed 把字节流拆成 UDP payload 并写入目标，返回成功写入的 payload 字节数。
func (f *udpFramer) feed(conn net.Conn, data []byte) (int, error) {
	f.buf = append(f.buf, data...)
	written := 0
	for {
		if len(f.buf) < 2 {
			return written, nil
		}
		n := int(binary.BigEndian.Uint16(f.buf[:2]))
		if len(f.buf) < 2+n {
			return written, nil
		}
		if n > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := conn.Write(f.buf[2 : 2+n]); err != nil {
				return written, err
			}
			written += n
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
