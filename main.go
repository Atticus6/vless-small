// Command vless-small 是最小可用的 VLESS over WebSocket 服务端.
//
// 只做三件事: WebSocket 握手、VLESS 头解析与 UUID 认证、TCP/UDP 双向转发.
// 不包含 TLS、隧道、管理后台; 需要加密时把本服务放在 Nginx/Cloudflare 之后.
package main

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// VLESS 协议常量.
const (
	vlessVersion = 0

	cmdTCP = 1
	cmdUDP = 2

	atypIPv4   = 1
	atypDomain = 2
	atypIPv6   = 3
)

// 转发调优常量.
const (
	dialTimeout    = 10 * time.Second
	writeTimeout   = 10 * time.Second
	readTimeout    = 60 * time.Second // WS 读超时, 由 pong 续期
	headerTimeout  = 30 * time.Second
	pingInterval   = 25 * time.Second
	udpIdleTimeout = 2 * time.Minute
	maxMessageSize = 256 * 1024
	bufferSize     = 32 * 1024
)

// server 持有只读的用户表与 WS 配置, 可被多个连接并发使用.
type server struct {
	users    map[[16]byte]struct{}
	path     string // 为空表示任意路径都接受
	upgrader websocket.Upgrader
}

func newServer(users map[[16]byte]struct{}, path string) *server {
	return &server{
		users: users,
		path:  path,
		upgrader: websocket.Upgrader{
			CheckOrigin:     func(*http.Request) bool { return true },
			ReadBufferSize:  4096,
			WriteBufferSize: 4096,
		},
	}
}

func main() {
	defaultListen := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		defaultListen = ":" + port
	}

	listen := flag.String("listen", defaultListen, "HTTP listen address")
	uuidList := flag.String("uuid", os.Getenv("UUID"), "comma-separated VLESS UUIDs")
	path := flag.String("path", os.Getenv("WS_PATH"), "WebSocket path to accept (empty = any)")
	flag.Parse()

	users, err := parseUsers(*uuidList)
	if err != nil {
		log.Fatalf("invalid users: %v", err)
	}
	if len(users) == 0 {
		log.Fatal("missing -uuid (or UUID env)")
	}

	wsPath := strings.TrimSpace(*path)
	if wsPath != "" && !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}

	srv := newServer(users, wsPath)
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           http.HandlerFunc(srv.handleWS),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Ctrl+C / SIGTERM 时优雅退出.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	displayPath := wsPath
	if displayPath == "" {
		displayPath = "*"
	}
	log.Printf("vless-ws listening on %s (path=%s, users=%d)", *listen, displayPath, len(users))
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// handleWS 完成握手、认证, 然后按命令分发到 TCP/UDP 转发.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
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
	ws.SetReadDeadline(time.Now().Add(headerTimeout))
	ws.SetPongHandler(func(string) error {
		return ws.SetReadDeadline(time.Now().Add(readTimeout))
	})

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
	if _, ok := s.users[id]; !ok {
		log.Printf("reject %s: unknown UUID", r.RemoteAddr)
		return
	}

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
func (s *server) relayTCP(ws *websocket.Conn, target string, early []byte) {
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
func (s *server) relayUDP(ws *websocket.Conn, target string, early []byte) {
	conn, err := net.DialTimeout("udp", target, dialTimeout)
	if err != nil {
		log.Printf("dial udp %s: %v", target, err)
		return
	}
	defer conn.Close()

	if err := writeMessage(ws, []byte{vlessVersion, 0}); err != nil {
		return
	}
	if len(early) > 0 {
		forwardUDP(conn, early)
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
				forwardUDP(conn, data)
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

// forwardUDP 把 [len][data] 拼接包逐个写到 UDP 远端.
func forwardUDP(conn net.Conn, data []byte) {
	for len(data) >= 2 {
		n := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if n > len(data) {
			log.Printf("drop truncated udp packet: want %d, have %d", n, len(data))
			return
		}
		if n > 0 {
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := conn.Write(data[:n]); err != nil {
				return
			}
		}
		data = data[n:]
	}
}

// writeMessage 写一条二进制 WS 消息; 调用方需保证只有单个 goroutine 写数据帧.
func writeMessage(ws *websocket.Conn, data []byte) error {
	_ = ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	return ws.WriteMessage(websocket.BinaryMessage, data)
}

// parseRequest 解析 VLESS 请求头.
//
//	+---------+----------+--------+---------+-------+----------+---------+
//	| Version |  UUID    | Addon  | Command | Port  | AddrType | Address |
//	| 1 byte  | 16 bytes | var    | 1 byte  | 2 B   | 1 byte   | var     |
//	+---------+----------+--------+---------+-------+----------+---------+
//
// 返回目标地址和头之后剩余的 early data.
func parseRequest(b []byte) (id [16]byte, cmd byte, target string, payload []byte, err error) {
	if len(b) < 18 {
		err = fmt.Errorf("header too short: %d", len(b))
		return
	}
	if b[0] != vlessVersion {
		err = fmt.Errorf("unsupported version: %d", b[0])
		return
	}
	copy(id[:], b[1:17])

	off := 18 + int(b[17]) // 跳过 addon
	if len(b) < off+4 {
		err = fmt.Errorf("header truncated before command")
		return
	}
	cmd = b[off]
	port := binary.BigEndian.Uint16(b[off+1 : off+3])
	atyp := b[off+3]
	off += 4

	var host string
	switch atyp {
	case atypIPv4:
		if len(b) < off+4 {
			err = fmt.Errorf("header truncated in IPv4 address")
			return
		}
		host = net.IP(b[off : off+4]).String()
		off += 4
	case atypDomain:
		if len(b) < off+1 {
			err = fmt.Errorf("header truncated in domain length")
			return
		}
		n := int(b[off])
		off++
		if len(b) < off+n {
			err = fmt.Errorf("header truncated in domain")
			return
		}
		host = string(b[off : off+n])
		off += n
	case atypIPv6:
		if len(b) < off+16 {
			err = fmt.Errorf("header truncated in IPv6 address")
			return
		}
		host = net.IP(b[off : off+16]).String()
		off += 16
	default:
		err = fmt.Errorf("unsupported address type: %d", atyp)
		return
	}
	if host == "" {
		err = fmt.Errorf("empty target host")
		return
	}

	target = net.JoinHostPort(host, strconv.Itoa(int(port)))
	payload = b[off:]
	return
}

// parseUsers 解析逗号分隔的 UUID 列表.
func parseUsers(raw string) (map[[16]byte]struct{}, error) {
	users := make(map[[16]byte]struct{})
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := parseUUID(part)
		if err != nil {
			return nil, err
		}
		users[id] = struct{}{}
	}
	return users, nil
}

// parseUUID 接受带或不带连字符的 UUID.
func parseUUID(s string) ([16]byte, error) {
	var id [16]byte
	hexStr := strings.ReplaceAll(strings.TrimSpace(s), "-", "")
	if len(hexStr) != 32 {
		return id, fmt.Errorf("invalid UUID %q", s)
	}
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return id, fmt.Errorf("invalid UUID %q: %w", s, err)
	}
	copy(id[:], b)
	return id, nil
}
