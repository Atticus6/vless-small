package hy2

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apernet/quic-go"
	"github.com/apernet/quic-go/http3"
	"github.com/apernet/quic-go/quicvarint"
)

// Config 是最小 Hysteria 2 服务端的启动参数。
type Config struct {
	// Listen 是 UDP 监听地址，例如 127.0.0.1:8081 或 :8443。
	Listen string
	// Password 是 Hysteria 客户端的认证密码，不能为空。
	Password string
	// CertFile / KeyFile 是 TLS 证书；Hysteria 强制 TLS 1.3 + ALPN h3。
	// 两者都为空时自动生成/复用自签证书（见 CertDir / SelfSignedDomain）。
	CertFile string
	KeyFile  string
	// CertDir 是自签证书的缓存目录；为空时使用二进制同目录的 .certs/hy2。
	CertDir string
	// SelfSignedDomain 是自签证书的 CN/SAN，默认 bing.com。
	SelfSignedDomain string
	// Dial 建立出站 TCP 连接；为空时使用带 10s 超时的 net.Dialer。
	// main 会注入 app.DialOutbound，以复用 DNS 缓存和 hedged dial。
	Dial func(ctx context.Context, network, target string) (net.Conn, error)
	// UDPIdleTimeout 是 UDP 会话空闲回收时间，默认 60s。
	UDPIdleTimeout time.Duration
	// Logf 是可选的日志函数。
	Logf func(format string, args ...any)
}

// Server 是一个最小的 Hysteria 2 服务端。
type Server struct {
	cfg      Config
	listener *quic.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	once     sync.Once
}

// New 校验配置并绑定 UDP 端口。
func New(cfg Config) (*Server, error) {
	cfg.Listen = strings.TrimSpace(cfg.Listen)
	if cfg.Listen == "" {
		return nil, errors.New("hy2: listen address is required")
	}
	if cfg.Password == "" {
		return nil, errors.New("hy2: password is required")
	}
	// 证书显式给出时必须成对出现；都不给则自动生成自签证书。
	if (cfg.CertFile == "") != (cfg.KeyFile == "") {
		return nil, errors.New("hy2: cert and key must be provided together")
	}
	if cfg.Dial == nil {
		cfg.Dial = func(ctx context.Context, network, target string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, target)
		}
	}
	if cfg.UDPIdleTimeout <= 0 {
		cfg.UDPIdleTimeout = 60 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}

	cert, err := loadCertificate(cfg)
	if err != nil {
		return nil, err
	}
	tlsConf := http3.ConfigureTLSConfig(&tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
	})
	quicConf := &quic.Config{
		EnableDatagrams: true,
		// 与 Hysteria 客户端默认行为配合：代理连接经常长时间空闲（看视频
		// 暂停、长连接保活），idle 超时太短会在空闲后被踢掉。保活间隔
		// 远小于 idle 超时，保证连接不被误判。
		MaxIdleTimeout:                 2 * time.Minute,
		KeepAlivePeriod:                15 * time.Second,
		MaxIncomingStreams:             4096,
		MaxIncomingUniStreams:          256,
		InitialStreamReceiveWindow:     4 << 20,
		MaxStreamReceiveWindow:         8 << 20,
		InitialConnectionReceiveWindow: 8 << 20,
		MaxConnectionReceiveWindow:     16 << 20,
	}
	listener, err := quic.ListenAddr(cfg.Listen, tlsConf, quicConf)
	if err != nil {
		return nil, fmt.Errorf("hy2: listen %s: %w", cfg.Listen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{cfg: cfg, listener: listener, ctx: ctx, cancel: cancel}, nil
}

// Addr 返回实际监听的 UDP 地址；端口为 0 时可用于测试。
func (s *Server) Addr() net.Addr {
	return s.listener.Addr()
}

// loadCertificate 返回显式配置的证书，或按需生成/复用自签证书。
//
// 自签证书用于“没有域名或不想配证书”的场景：客户端需要设置
// `tls.insecure: true`（或把生成的证书加进信任链）。
func loadCertificate(cfg Config) (tls.Certificate, error) {
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("hy2: load certificate: %w", err)
		}
		return cert, nil
	}

	dir := cfg.CertDir
	if dir == "" {
		dir = defaultCertDir()
	}
	domain := cfg.SelfSignedDomain
	if domain == "" {
		domain = defaultSelfSignedDomain
	}
	cert, err := loadOrCreateSelfSigned(dir, domain)
	if err != nil {
		return tls.Certificate{}, err
	}
	cfg.Logf("hy2: using self-signed certificate for %s (%s)", domain, dir)
	return cert, nil
}

// Serve 接受 QUIC 连接，直到 Close 被调用。
func (s *Server) Serve() error {
	for {
		conn, err := s.listener.Accept(s.ctx)
		if err != nil {
			if s.ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handleConn(conn)
	}
}

// Close 停止监听并关闭所有连接。
func (s *Server) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.listener.Close()
	})
	return nil
}

func (s *Server) handleConn(conn *quic.Conn) {
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()

	h := &connHandler{srv: s, conn: conn, ctx: ctx}
	h3 := &http3.Server{
		Handler:          h,
		StreamDispatcher: h.dispatch,
		// HTTP/3 层不额外设空闲超时：连接活跃度由 QUIC 的 idle 与保活决定，
		// 两处都设短超时会让长连接被较早的一方踢掉。
	}
	err := h3.ServeQUICConn(conn)
	h.closeSessions()
	_ = conn.CloseWithError(0, "")
	if err != nil && ctx.Err() == nil {
		s.cfg.Logf("hy2: connection %s: %v", conn.RemoteAddr(), err)
	}
}

// connHandler 处理一条 QUIC 连接上的认证、TCP 流和 UDP datagram。
type connHandler struct {
	srv  *Server
	conn *quic.Conn
	ctx  context.Context

	authenticated atomic.Bool
	udpOnce       sync.Once
	sessions      *udpSessionManager
}

// ServeHTTP 只处理 Hysteria 的认证请求；其它请求返回 404。
//
// 认证成功后返回 233，并告知客户端 UDP 可用、拥塞控制由客户端自行决定
// （Hysteria-CC-RX: auto）——本实现不提供 Brutal。
func (h *connHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.EqualFold(r.Host, authHost) ||
		r.URL.Path != authPath {
		http.NotFound(w, r)
		return
	}
	if h.authenticated.Load() {
		h.writeAuthOK(w)
		return
	}

	got := r.Header.Get(headerAuth)
	if subtle.ConstantTimeCompare([]byte(got), []byte(h.srv.cfg.Password)) != 1 {
		// 与 Hysteria 一致：认证失败时表现得像普通 Web 服务。
		http.NotFound(w, r)
		return
	}
	h.authenticated.Store(true)
	h.writeAuthOK(w)
	h.srv.cfg.Logf("hy2: authenticated client %s", h.conn.RemoteAddr())

	h.udpOnce.Do(func() {
		h.sessions = newUDPSessionManager(h)
		go h.udpLoop()
		go h.sessions.run(h.ctx)
	})
}

func (h *connHandler) writeAuthOK(w http.ResponseWriter) {
	w.Header().Set(headerUDPEnabled, "true")
	w.Header().Set(headerCCRX, "auto")
	w.Header().Set(headerPadding, randomPadding(16))
	w.WriteHeader(statusAuthOK)
}

// dispatch 接管认证后的 TCP 数据流（帧类型 0x401）。
//
// StreamDispatcher 只 peek 了帧类型，因此这里先消费掉这个 varint，
// 后续 readTCPRequest 才能从地址长度开始解析。
func (h *connHandler) dispatch(ft http3.FrameType, stream *quic.Stream, err error) (bool, error) {
	if err != nil || !h.authenticated.Load() {
		return false, nil
	}
	if ft != frameTypeTCPRequest {
		return false, nil
	}
	if _, err := quicvarint.Read(quicvarint.NewReader(stream)); err != nil {
		return true, err
	}
	go h.handleTCP(stream)
	return true, nil
}

func (h *connHandler) handleTCP(stream *quic.Stream) {
	defer stream.Close()

	addr, err := readTCPRequest(stream)
	if err != nil {
		_ = writeTCPResponse(stream, false, err.Error())
		return
	}

	dialCtx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
	target, err := h.srv.cfg.Dial(dialCtx, "tcp", addr)
	cancel()
	if err != nil {
		_ = writeTCPResponse(stream, false, err.Error())
		return
	}
	defer target.Close()

	if err := writeTCPResponse(stream, true, "Connected"); err != nil {
		return
	}
	relay(stream, target)
}

// relayBufferPool 复用 TCP 中继缓冲。
var relayBufferPool = sync.Pool{
	New: func() any {
		b := make([]byte, 32*1024)
		return &b
	},
}

// relay 在 QUIC 流和出站 TCP 连接之间双向复制。
//
// 任一方向结束即关闭两端；与 Hysteria 的 copyTwoWay 行为一致，避免一条
// 半关闭的连接长期占着流。
func relay(stream *quic.Stream, target net.Conn) {
	done := make(chan struct{}, 2)

	go func() {
		bufp := relayBufferPool.Get().(*[]byte)
		_, _ = io.CopyBuffer(target, stream, *bufp)
		relayBufferPool.Put(bufp)
		done <- struct{}{}
	}()
	go func() {
		bufp := relayBufferPool.Get().(*[]byte)
		_, _ = io.CopyBuffer(stream, target, *bufp)
		relayBufferPool.Put(bufp)
		done <- struct{}{}
	}()

	<-done
	// Close 只结束写方向；必须 CancelRead 才能唤醒另一个方向。
	stream.CancelRead(0)
	_ = stream.Close()
	_ = target.Close()
}

// udpLoop 持续接收客户端发来的 QUIC datagram。
func (h *connHandler) udpLoop() {
	for {
		data, err := h.conn.ReceiveDatagram(h.ctx)
		if err != nil {
			return
		}
		msg, err := parseUDPMessage(data)
		if err != nil {
			h.srv.cfg.Logf("hy2: bad UDP message: %v", err)
			continue
		}
		h.sessions.feed(msg)
	}
}

// sendUDPMessage 发送一条 UDP 消息；超过 datagram 上限时自动分片。
func (h *connHandler) sendUDPMessage(msg *udpMessage) error {
	if err := h.conn.SendDatagram(msg.appendTo(nil)); err == nil {
		return nil
	} else {
		var tooLarge *quic.DatagramTooLargeError
		if !errors.As(err, &tooLarge) {
			return err
		}
		msg.PacketID = uint16(time.Now().UnixNano())
		for _, frag := range fragmentUDPMessage(msg, int(tooLarge.MaxDatagramPayloadSize)) {
			if err := h.conn.SendDatagram(frag.appendTo(nil)); err != nil {
				return err
			}
		}
		return nil
	}
}

func (h *connHandler) closeSessions() {
	if h.sessions != nil {
		h.sessions.closeAll()
	}
}
