// Command vless-small 是最小可用的 VLESS over WebSocket 服务端.
//
// 只做三件事: WebSocket 握手、VLESS 头解析与 UUID 认证、TCP/UDP 双向转发.
// 不包含 TLS、隧道、管理后台; 需要加密时把本服务放在 Nginx/Cloudflare 之后.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync/atomic"
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

	// 从主控拉取设备的间隔.
	nodeSyncInterval = 60 * time.Second
)

// XHTTP 默认路径。主控订阅固定下发 /xhttp，节点默认保持一致；
// 需要关闭时显式设置 XHTTP_PATH=none（或 off）。
const (
	defaultXHTTPPath   = "/xhttp"
	xhttpDisabledValue = "none"
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

func main() {
	defaultListen := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		defaultListen = ":" + port
	}

	listen := flag.String("listen", defaultListen, "HTTP listen address")
	uuidList := flag.String("uuid", os.Getenv("UUID"), "comma-separated VLESS UUIDs")
	path := flag.String("path", os.Getenv("WS_PATH"), "WebSocket path to accept (empty = any)")
	xhttpPathFlag := flag.String(
		"xhttp-path",
		os.Getenv("XHTTP_PATH"),
		"XHTTP stream-one path (default /xhttp; use none to disable)",
	)
	config := flag.String("config", os.Getenv("CONFIG"), "control plane: <baseURL>_<name>_<token>")
	flag.Parse()

	nodeCfg, err := parseNodeConfig(*config)
	if err != nil {
		log.Fatalf("invalid CONFIG: %v", err)
	}

	users, err := parseUsers(*uuidList)
	if err != nil {
		log.Fatalf("invalid users: %v", err)
	}
	if len(users) == 0 && nodeCfg.baseURL == "" {
		log.Fatal("missing -uuid (or UUID env) or -config (or CONFIG env)")
	}

	wsPath := strings.TrimSpace(*path)
	if wsPath != "" && !strings.HasPrefix(wsPath, "/") {
		wsPath = "/" + wsPath
	}
	xhttpPath := resolveXHTTPPath(*xhttpPathFlag)
	if xhttpPath != "" && xhttpPath == wsPath {
		log.Fatal("XHTTP_PATH must differ from WS_PATH")
	}

	srv := newServer(users, wsPath)
	srv.xhttpPath = xhttpPath
	if nodeCfg.baseURL != "" {
		info := collectNodeInfo()
		log.Printf(
			"node info runtime=%s hosts=%v ipv4=%s ipv6=%s country=%s",
			info.runtime, info.hosts, info.ipv4, info.ipv6, info.countryCode,
		)
		if err := syncDevices(nodeCfg, srv, *listen, wsPath, users, info); err != nil {
			log.Printf("initial device sync failed: %v", err)
		}

		// Vercel / Railway 无请求时会自动休眠；后台定时同步会产生持续请求，
		// 让实例无法休眠。这两个平台只在冷启动时同步一次。
		if isAutoSleepRuntime(info.runtime) {
			log.Printf("runtime %s: background sync disabled (auto-sleep)", info.runtime)
		} else {
			go syncLoop(nodeCfg, srv, *listen, wsPath, users, info)
		}
	}
	httpServer := &http.Server{
		Addr:              *listen,
		Handler:           http.HandlerFunc(srv.handleHTTP),
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Ctrl+C / SIGTERM 时优雅退出.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
		// Shutdown 不跟踪 hijack 后的 WebSocket, 再给转发会话一段排空时间.
		srv.waitDrain(5 * time.Second)
	}()

	displayPath := wsPath
	if displayPath == "" {
		displayPath = "*"
	}
	displayXHTTP := xhttpPath
	if displayXHTTP == "" {
		displayXHTTP = "disabled"
	}
	log.Printf(
		"vless-ws listening on %s (path=%s, xhttp=%s, users=%d)",
		*listen, displayPath, displayXHTTP, srv.userCount(),
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-shutdownDone
}

// resolveXHTTPPath 解析 XHTTP 路径。
//
// 空值使用默认 /xhttp（主控订阅默认包含 XHTTP 节点）；
// none/off/- 用于显式禁用，其他值按路径规范化。
func resolveXHTTPPath(raw string) string {
	value := strings.ToLower(strings.TrimSpace(raw))
	switch value {
	case "":
		return defaultXHTTPPath
	case xhttpDisabledValue, "off", "-":
		return ""
	default:
		return normalizeHTTPPath(raw)
	}
}

// normalizeHTTPPath 统一路径格式：确保以 / 开头，去掉尾部斜杠.
func normalizeHTTPPath(raw string) string {
	path := strings.TrimSpace(raw)
	if path == "" {
		return ""
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	if path == "" {
		return "/"
	}
	return path
}

// waitDrain 等待已升级的 WebSocket 会话结束, 最多等 timeout.
func (s *server) waitDrain(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for s.active.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
}

// handleWS 完成握手、认证, 然后按命令分发到 TCP/UDP 转发.
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	s.active.Add(1)
	defer s.active.Add(-1)

	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		// 浏览器/探活请求走普通 HTTP：根路径和 /healthz 返回健康信息，
		// 避免直接看到 426/400；真正的 WebSocket 路径仍要求 Upgrade。
		if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
			uptime := time.Since(s.startedAt)

			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			_ = json.NewEncoder(w).Encode(healthResponse{
				OK:            true,
				Uptime:        uptime.Truncate(time.Second).String(),
				UptimeSeconds: int64(uptime.Seconds()),
			})
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("vless-small ok\n"))
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

// handleHTTP 按路径分发 WebSocket / XHTTP stream-one 请求.
func (s *server) handleHTTP(w http.ResponseWriter, r *http.Request) {
	if s.xhttpPath != "" && strings.HasPrefix(r.URL.Path, s.xhttpPath) {
		s.handleXHTTP(w, r)
		return
	}
	s.handleWS(w, r)
}

// handleXHTTP 实现 XHTTP stream-one：一个 HTTP 请求同时承载上下行，
// 不拆分 upload/download 请求。请求体是 VLESS 上行流，响应体是下行流.
func (s *server) handleXHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodPut {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}

	reader := bufio.NewReader(r.Body)
	id, cmd, target, err := parseRequestStream(reader)
	if err != nil {
		log.Printf("xhttp bad request %s: %v", r.RemoteAddr, err)
		http.Error(w, "bad vless header", http.StatusBadRequest)
		return
	}
	if !s.hasUser(id) {
		log.Printf("xhttp reject %s: unknown UUID", r.RemoteAddr)
		http.Error(w, "unknown UUID", http.StatusForbidden)
		return
	}

	s.active.Add(1)
	defer s.active.Add(-1)

	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	writer := &flushWriter{Writer: w, flusher: flusher}

	switch cmd {
	case cmdTCP:
		s.relayTCPStream(r.Context(), reader, writer, target)
	case cmdUDP:
		s.relayUDPStream(r.Context(), reader, writer, target)
	default:
		log.Printf("xhttp unsupported command %d from %s", cmd, r.RemoteAddr)
	}
}

// flushWriter 每次写入后 flush，避免反向代理缓冲 XHTTP 下行.
type flushWriter struct {
	io.Writer
	flusher http.Flusher
}

func (w *flushWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if w.flusher != nil {
		w.flusher.Flush()
	}
	return n, err
}

// relayTCPStream: XHTTP 字节流 <-> 目标 TCP.
func (s *server) relayTCPStream(
	ctx context.Context,
	reader io.Reader,
	writer io.Writer,
	target string,
) {
	conn, err := net.DialTimeout("tcp", target, dialTimeout)
	if err != nil {
		log.Printf("xhttp dial %s: %v", target, err)
		return
	}
	defer conn.Close()

	// 客户端断开 HTTP 请求时主动关闭目标连接，避免等待读超时。
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	// VLESS 响应头: version + addon 长度 0.
	if _, err := writer.Write([]byte{vlessVersion, 0}); err != nil {
		return
	}

	done := make(chan bool, 2)
	go func() {
		_, _ = io.Copy(conn, reader)
		done <- true
	}()
	go func() {
		_, _ = io.Copy(writer, conn)
		done <- false
	}()

	if <-done {
		// 上行 EOF 后不调用 CloseWrite：XHTTP 经过 Clash TUN、Vercel 等中间层时，
		// 半关闭常被当成整条连接关闭，目标响应还没读回就被丢弃。
		// stream-one 的上行结束通常意味着整个会话结束，保持读方向直到目标自然关闭即可。
		_ = conn.SetReadDeadline(time.Now().Add(readTimeout))
		<-done
	}
}

// relayUDPStream: XHTTP 字节流 <-> 目标 UDP.
// 上行包格式与 VLESS UDP 一致: [2B 大端长度][payload].
func (s *server) relayUDPStream(
	ctx context.Context,
	reader io.Reader,
	writer io.Writer,
	target string,
) {
	conn, err := net.DialTimeout("udp", target, dialTimeout)
	if err != nil {
		log.Printf("xhttp dial udp %s: %v", target, err)
		return
	}
	defer conn.Close()

	// 客户端断开 HTTP 请求时主动关闭目标连接.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	if _, err := writer.Write([]byte{vlessVersion, 0}); err != nil {
		return
	}

	go func() {
		defer conn.Close()
		for {
			var lenBuf [2]byte
			if _, err := io.ReadFull(reader, lenBuf[:]); err != nil {
				return
			}
			n := int(binary.BigEndian.Uint16(lenBuf[:]))
			payload := make([]byte, n)
			if _, err := io.ReadFull(reader, payload); err != nil {
				return
			}
			_ = conn.SetWriteDeadline(time.Now().Add(writeTimeout))
			if _, err := conn.Write(payload); err != nil {
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(udpIdleTimeout))
		}
	}()

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
		if _, err := writer.Write(buf[:2+n]); err != nil {
			return
		}
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

// parseRequestStream 从字节流中读取 VLESS 请求头.
// 与 parseRequest 的区别是：只消费头部，early data 留在 bufio.Reader 中，
// 供后续 TCP/UDP relay 直接读取.
func parseRequestStream(r *bufio.Reader) (id [16]byte, cmd byte, target string, err error) {
	head := make([]byte, 18)
	if _, err = io.ReadFull(r, head); err != nil {
		return
	}
	if head[0] != vlessVersion {
		err = fmt.Errorf("unsupported version: %d", head[0])
		return
	}
	copy(id[:], head[1:17])

	addonLen := int(head[17])
	if addonLen > 0 {
		if _, err = io.CopyN(io.Discard, r, int64(addonLen)); err != nil {
			return
		}
	}

	fixed := make([]byte, 4)
	if _, err = io.ReadFull(r, fixed); err != nil {
		return
	}
	cmd = fixed[0]
	port := binary.BigEndian.Uint16(fixed[1:3])
	atyp := fixed[3]

	var host string
	switch atyp {
	case atypIPv4:
		b := make([]byte, 4)
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case atypDomain:
		n, readErr := r.ReadByte()
		if readErr != nil {
			err = readErr
			return
		}
		b := make([]byte, int(n))
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = string(b)
	case atypIPv6:
		b := make([]byte, 16)
		if _, err = io.ReadFull(r, b); err != nil {
			return
		}
		host = net.IP(b).String()
	default:
		err = fmt.Errorf("unsupported address type: %d", atyp)
		return
	}
	if host == "" {
		err = fmt.Errorf("empty target host")
		return
	}

	target = net.JoinHostPort(host, strconv.Itoa(int(port)))
	return
}

// nodeConfig 是 CONFIG=<baseURL>_<name>_<token> 的解析结果.
// name 与 token 中不要包含下划线.
type nodeConfig struct {
	baseURL string
	name    string
	token   string
}

func parseNodeConfig(raw string) (nodeConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nodeConfig{}, nil
	}

	last := strings.LastIndex(raw, "_")
	if last < 0 {
		return nodeConfig{}, fmt.Errorf("expected <baseURL>_<name>_<token>")
	}
	token := strings.TrimSpace(raw[last+1:])
	rest := raw[:last]

	sep := strings.LastIndex(rest, "_")
	if sep < 0 {
		return nodeConfig{}, fmt.Errorf("expected <baseURL>_<name>_<token>")
	}
	name := strings.TrimSpace(rest[sep+1:])
	baseURL := strings.TrimRight(strings.TrimSpace(rest[:sep]), "/")

	if baseURL == "" || name == "" || token == "" {
		return nodeConfig{}, fmt.Errorf("CONFIG requires baseURL, name and token")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return nodeConfig{}, fmt.Errorf("baseURL must start with http:// or https://")
	}

	return nodeConfig{baseURL: baseURL, name: name, token: token}, nil
}

type nodeDevice struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
}

type nodeReportResponse struct {
	Devices []nodeDevice `json:"devices"`
}

var syncHTTPClient = &http.Client{Timeout: 10 * time.Second}

type nodeInfo struct {
	runtime     string
	hosts       []string
	ipv4        string
	ipv6        string
	countryCode string
	latitude    *float64
	longitude   *float64
}

type nodeReport struct {
	Name        string   `json:"name"`
	Runtime     string   `json:"runtime"`
	Listen      string   `json:"listen,omitempty"`
	Path        string   `json:"path,omitempty"`
	Hosts       []string `json:"hosts,omitempty"`
	IPv4        string   `json:"ipv4,omitempty"`
	IPv6        string   `json:"ipv6,omitempty"`
	CountryCode string   `json:"countryCode,omitempty"`
	Latitude    *float64 `json:"latitude,omitempty"`
	Longitude   *float64 `json:"longitude,omitempty"`
}

// collectNodeInfo 汇总平台信息与本机公网 IP/地理位置.
func collectNodeInfo() nodeInfo {
	info := nodeInfo{
		runtime: detectRuntime(),
		hosts:   detectHosts(),
	}

	found, err := lookupIPInfo()
	if err != nil {
		log.Printf("ip lookup failed: %v", err)
	} else {
		info.ipv4 = found.ipv4
		info.ipv6 = found.ipv6
		info.countryCode = found.countryCode
		info.latitude = found.latitude
		info.longitude = found.longitude
	}

	if v := strings.TrimSpace(os.Getenv("NODE_IPV4")); v != "" {
		info.ipv4 = v
	}
	if v := strings.TrimSpace(os.Getenv("NODE_IPV6")); v != "" {
		info.ipv6 = v
	}
	return info
}

// detectRuntime 通过环境变量判断当前 SaaS 平台.
func detectRuntime() string {
	if v := firstNonEmpty(os.Getenv("NODE_RUNTIME"), os.Getenv("RUNTIME")); v != "" {
		return v
	}

	switch {
	case envSet("VERCEL") || envSet("VERCEL_URL"):
		return "vercel"
	case envSet("RAILWAY_ENVIRONMENT") || envSet("RAILWAY_PUBLIC_DOMAIN"):
		return "railway"
	case envSet("RENDER") || envSet("RENDER_EXTERNAL_URL"):
		return "render"
	case envSet("FLY_APP_NAME"):
		return "fly"
	case envSet("KOYEB_APP_NAME") || envSet("KOYEB_PUBLIC_DOMAIN"):
		return "koyeb"
	case envSet("CF_PAGES") || envSet("CF_PAGES_URL"):
		return "cloudflare"
	case envSet("ZEABUR") || envSet("ZEABUR_WEB_URL"):
		return "zeabur"
	case envSet("NORTHFLANK") || envPrefixSet("NORTHFLANK_"):
		return "northflank"
	default:
		return "local"
	}
}

func envSet(key string) bool {
	return strings.TrimSpace(os.Getenv(key)) != ""
}

// envPrefixSet 检查是否存在以 prefix 开头的环境变量。
// Northflank 没有固定的运行 URL 变量，但会/可以由用户注入 NORTHFLANK_* 变量。
func envPrefixSet(prefix string) bool {
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

// isAutoSleepRuntime 判断平台是否会在没有请求时自动休眠。
// 这类平台不能跑后台轮询，否则持续请求会阻止实例休眠。
// Northflank 可以常驻，因此保持后台同步。
func isAutoSleepRuntime(runtime string) bool {
	return runtime == "vercel" || runtime == "railway"
}

// detectHosts 获取平台分配的公网地址.
func detectHosts() []string {
	if raw := os.Getenv("PUBLIC_HOSTS"); strings.TrimSpace(raw) != "" {
		return normalizeHosts(strings.Split(raw, ","))
	}
	if raw := os.Getenv("PUBLIC_HOST"); strings.TrimSpace(raw) != "" {
		return normalizeHosts([]string{raw})
	}

	// 只使用稳定的公网地址：
	// - 不用 VERCEL_URL：它是每次部署都会变化的 deployment URL；
	// - 不用 CF_PAGES_URL：它同样指向单次部署，不是稳定生产域名。
	candidates := []string{
		os.Getenv("VERCEL_PROJECT_PRODUCTION_URL"),
		os.Getenv("RAILWAY_PUBLIC_DOMAIN"),
		os.Getenv("RENDER_EXTERNAL_HOSTNAME"),
		os.Getenv("RENDER_EXTERNAL_URL"),
		os.Getenv("KOYEB_PUBLIC_DOMAIN"),
		os.Getenv("ZEABUR_WEB_URL"),
		// Northflank 不注入自身 .code.run URL，通常由部署者手动设置这些变量。
		os.Getenv("NORTHFLANK_PUBLIC_URL"),
		os.Getenv("NORTHFLANK_SERVICE_URL"),
		os.Getenv("NORTHFLANK_URL"),
		os.Getenv("NORTHFLANK_HOST"),
		os.Getenv("NORTHFLANK_PUBLIC_HOST"),
		os.Getenv("NORTHFLANK_SERVICE_HOST"),
		os.Getenv("NORTHFLANK_DOMAIN"),
		os.Getenv("NORTHFLANK_SERVICE_DOMAIN"),
		os.Getenv("SERVICE_URL"),
		os.Getenv("PUBLIC_URL"),
	}
	if app := strings.TrimSpace(os.Getenv("FLY_APP_NAME")); app != "" {
		candidates = append(candidates, app+".fly.dev")
	}
	return normalizeHosts(candidates)
}

func normalizeHosts(raw []string) []string {
	seen := make(map[string]struct{}, len(raw))
	hosts := make([]string, 0, len(raw))

	for _, item := range raw {
		host := normalizeHost(item)
		if host == "" {
			continue
		}
		if _, ok := seen[host]; ok {
			continue
		}
		seen[host] = struct{}{}
		hosts = append(hosts, host)
	}
	return hosts
}

func normalizeHost(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.Contains(raw, "://") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(u.Host)
	}
	if i := strings.IndexAny(raw, "/?#"); i >= 0 {
		raw = raw[:i]
	}
	return strings.TrimSpace(raw)
}

type ipLookupResponse struct {
	IP          string   `json:"ip"`
	IPv4        string   `json:"ipv4"`
	IPv6        string   `json:"ipv6"`
	Type        string   `json:"type"`
	Country     string   `json:"country"`
	CountryCode string   `json:"country_code"`
	Loc         string   `json:"loc"`
	Latitude    *float64 `json:"latitude"`
	Longitude   *float64 `json:"longitude"`
	Success     *bool    `json:"success"`
}

func lookupIPInfo() (nodeInfo, error) {
	endpoint := strings.TrimSpace(os.Getenv("IP_LOOKUP_URL"))
	if endpoint == "" {
		endpoint = "https://ipinfo.io/json"
	}

	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nodeInfo{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "vless-small")

	resp, err := syncHTTPClient.Do(req)
	if err != nil {
		return nodeInfo{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nodeInfo{}, fmt.Errorf("lookup %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var data ipLookupResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&data); err != nil {
		return nodeInfo{}, fmt.Errorf("decode lookup: %w", err)
	}
	if data.Success != nil && !*data.Success {
		return nodeInfo{}, fmt.Errorf("lookup returned success=false")
	}

	info := nodeInfo{
		ipv4:        strings.TrimSpace(data.IPv4),
		ipv6:        strings.TrimSpace(data.IPv6),
		countryCode: strings.ToUpper(firstNonEmpty(data.CountryCode, data.Country)),
		latitude:    data.Latitude,
		longitude:   data.Longitude,
	}

	ip := strings.TrimSpace(data.IP)
	if ip != "" {
		parsed := net.ParseIP(ip)
		switch {
		case parsed == nil:
		case parsed.To4() != nil:
			if info.ipv4 == "" {
				info.ipv4 = ip
			}
		default:
			if info.ipv6 == "" {
				info.ipv6 = ip
			}
		}
	}
	if strings.EqualFold(data.Type, "ipv4") && info.ipv4 == "" {
		info.ipv4 = ip
	}
	if strings.EqualFold(data.Type, "ipv6") && info.ipv6 == "" {
		info.ipv6 = ip
	}

	if info.latitude == nil || info.longitude == nil {
		if lat, lon, ok := parseLoc(data.Loc); ok {
			info.latitude, info.longitude = &lat, &lon
		}
	}
	return info, nil
}

func parseLoc(raw string) (float64, float64, bool) {
	parts := strings.Split(raw, ",")
	if len(parts) != 2 {
		return 0, 0, false
	}
	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return 0, 0, false
	}
	lon, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return 0, 0, false
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return 0, 0, false
	}
	return lat, lon, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func syncLoop(
	cfg nodeConfig,
	srv *server,
	listen, path string,
	static map[[16]byte]struct{},
	info nodeInfo,
) {
	ticker := time.NewTicker(nodeSyncInterval)
	defer ticker.Stop()

	for range ticker.C {
		if err := syncDevices(cfg, srv, listen, path, static, info); err != nil {
			log.Printf("device sync failed: %v", err)
		}
	}
}

// syncDevices 向主控上报节点信息, 并把返回的所有启用设备设为可认证 UUID.
func syncDevices(
	cfg nodeConfig,
	srv *server,
	listen, path string,
	static map[[16]byte]struct{},
	info nodeInfo,
) error {
	payload, err := json.Marshal(nodeReport{
		Name:        cfg.name,
		Runtime:     info.runtime,
		Listen:      listen,
		Path:        path,
		Hosts:       info.hosts,
		IPv4:        info.ipv4,
		IPv6:        info.ipv6,
		CountryCode: info.countryCode,
		Latitude:    info.latitude,
		Longitude:   info.longitude,
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, cfg.baseURL+"/api/nodes/report", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.token)

	resp, err := syncHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("report %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var result nodeReportResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}

	users := make(map[[16]byte]struct{}, len(static)+len(result.Devices))
	for id := range static {
		users[id] = struct{}{}
	}
	for _, device := range result.Devices {
		if !device.Enabled {
			continue
		}
		id, err := parseUUID(device.ID)
		if err != nil {
			log.Printf("skip device %q: %v", device.Name, err)
			continue
		}
		users[id] = struct{}{}
	}

	srv.setUsers(users)
	log.Printf("synced %d devices from %s", len(users), cfg.baseURL)
	return nil
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
