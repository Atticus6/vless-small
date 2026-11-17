package main

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

var errXHTTPQueueFull = errors.New("xhttp upload queue full")

// xhttpUploadQueue 按 seq 重排 packet-up 上传分片，并把连续字节流暴露给
// VLESS header 解析和 TCP/UDP relay.
//
// packet-up 的每个 POST 都是独立请求，可能乱序到达；Read 只返回 nextSeq
// 开始连续的数据，缺失的序号会阻塞等待。
type xhttpUploadQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	next   uint64
	buf    []byte
	chunks map[uint64][]byte
	bytes  int
	closed bool
}

func newXHTTPUploadQueue() *xhttpUploadQueue {
	q := &xhttpUploadQueue{chunks: make(map[uint64][]byte)}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *xhttpUploadQueue) Push(seq uint64, payload []byte) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.closed {
		return io.ErrClosedPipe
	}
	if seq < q.next {
		return nil // 重复/过期分片，直接忽略
	}
	if _, exists := q.chunks[seq]; exists {
		return nil
	}
	if len(q.chunks) >= xhttpMaxBufferedPackets || q.bytes+len(payload) > xhttpMaxBufferedBytes {
		return errXHTTPQueueFull
	}

	q.chunks[seq] = payload
	q.bytes += len(payload)
	q.cond.Broadcast()
	return nil
}

func (q *xhttpUploadQueue) Read(p []byte) (int, error) {
	q.mu.Lock()

	for {
		if len(q.buf) > 0 {
			n := copy(p, q.buf)
			q.buf = q.buf[n:]
			q.bytes -= n
			q.cond.Broadcast()
			q.mu.Unlock()
			return n, nil
		}

		if payload, ok := q.chunks[q.next]; ok {
			delete(q.chunks, q.next)
			q.next++
			q.buf = payload
			q.cond.Broadcast()
			continue
		}

		if q.closed {
			q.mu.Unlock()
			return 0, io.EOF
		}

		q.cond.Wait()
	}
}

func (q *xhttpUploadQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}

// xhttpSession 表示一次 packet-up 会话：下载 GET 与若干上传 POST 共享同一个 session ID.
type xhttpSession struct {
	id        string
	queue     *xhttpUploadQueue
	connected chan struct{}
	once      sync.Once
	closed    chan struct{}
	closeOnce sync.Once
}

func newXHTTPSession(id string) *xhttpSession {
	return &xhttpSession{
		id:        id,
		queue:     newXHTTPUploadQueue(),
		connected: make(chan struct{}),
		closed:    make(chan struct{}),
	}
}

// markConnected 让 30 秒 reaper 在下载请求接入后停止。
func (s *xhttpSession) markConnected() {
	s.once.Do(func() { close(s.connected) })
}

func (s *xhttpSession) isConnected() bool {
	select {
	case <-s.connected:
		return true
	default:
		return false
	}
}

func (s *xhttpSession) close() {
	s.closeOnce.Do(func() {
		s.queue.Close()
		close(s.closed)
	})
}

// xhttpSubpath 把请求路径拆成 XHTTP 子路径段.
// 例如 /xhttp/<sid>/<seq> → ["<sid>", "<seq>"]；/xhttp → [].
func (s *server) xhttpSubpath(path string) ([]string, bool) {
	if s.xhttpPath == "" {
		return nil, false
	}

	base := strings.TrimRight(s.xhttpPath, "/")
	if path == s.xhttpPath || (base != "" && path == base) {
		return nil, true
	}

	prefix := base + "/"
	if base == "" {
		prefix = "/"
	}
	if !strings.HasPrefix(path, prefix) {
		return nil, false
	}

	rest := strings.Trim(strings.TrimPrefix(path, prefix), "/")
	if rest == "" {
		return nil, true
	}
	return strings.Split(rest, "/"), true
}

func (s *server) getXHTTPSession(id string) *xhttpSession {
	s.sessionsMu.Lock()
	defer s.sessionsMu.Unlock()
	return s.sessions[id]
}

// getOrCreateXHTTPSession 获取或创建会话，并启动 30 秒 reaper.
// packet-up 客户端会先发下载 GET 再发上传 POST，也可能相反，两种顺序都支持。
// 新建会话超过上限时返回 false，避免随机 session 撑爆内存。
func (s *server) getOrCreateXHTTPSession(id string) (*xhttpSession, bool) {
	s.sessionsMu.Lock()
	if session, ok := s.sessions[id]; ok {
		s.sessionsMu.Unlock()
		return session, true
	}
	if len(s.sessions) >= xhttpMaxSessions {
		s.sessionsMu.Unlock()
		return nil, false
	}

	session := newXHTTPSession(id)
	s.sessions[id] = session
	s.sessionsMu.Unlock()

	go func() {
		timer := time.NewTimer(xhttpSessionReapTimeout)
		defer timer.Stop()

		select {
		case <-timer.C:
		case <-session.connected:
			return
		case <-session.closed:
			return
		}

		s.sessionsMu.Lock()
		defer s.sessionsMu.Unlock()
		if s.sessions[id] == session && !session.isConnected() {
			delete(s.sessions, id)
			session.close()
		}
	}()

	return session, true
}

func (s *server) dropXHTTPSession(id string, session *xhttpSession) {
	s.sessionsMu.Lock()
	if s.sessions[id] == session {
		delete(s.sessions, id)
		session.close()
	}
	s.sessionsMu.Unlock()
}

// validXHTTPSessionID 只接受可打印 ASCII 的路径段，避免异常 session 键.
func validXHTTPSessionID(id string) bool {
	if len(id) < 8 || len(id) > 256 {
		return false
	}
	for _, c := range id {
		if c < 0x21 || c > 0x7e || c == '/' {
			return false
		}
	}
	return true
}

// handleXHTTPUpload 接收 packet-up 的一个上传分片.
// 客户端按 seq 递增发送，服务端负责重排；返回 200 后客户端继续发下一片.
func (s *server) handleXHTTPUpload(
	w http.ResponseWriter,
	r *http.Request,
	sessionID, seqText string,
) {
	if !validXHTTPSessionID(sessionID) {
		http.Error(w, "invalid session", http.StatusBadRequest)
		return
	}
	seq, err := strconv.ParseUint(seqText, 10, 64)
	if err != nil {
		http.Error(w, "invalid seq", http.StatusBadRequest)
		return
	}
	if r.ContentLength > xhttpPacketMaxBytes {
		http.Error(w, "packet too large", http.StatusRequestEntityTooLarge)
		return
	}

	payload, err := io.ReadAll(io.LimitReader(r.Body, xhttpPacketMaxBytes+1))
	if err != nil {
		http.Error(w, "read packet failed", http.StatusBadRequest)
		return
	}
	if len(payload) > xhttpPacketMaxBytes {
		http.Error(w, "packet too large", http.StatusRequestEntityTooLarge)
		return
	}

	session, ok := s.getOrCreateXHTTPSession(sessionID)
	if !ok {
		http.Error(w, "too many xhttp sessions", http.StatusServiceUnavailable)
		return
	}
	if err := session.queue.Push(seq, payload); err != nil {
		log.Printf("xhttp packet-up push %s seq=%d: %v", sessionID, seq, err)
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}

	// 空 body 的 POST 可能被 CDN 缓存，明确禁止.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// handleXHTTPDownload 处理 packet-up 的下载 GET.
// 响应头先返回，随后等待第一个上传分片里的 VLESS 头并建立目标连接.
func (s *server) handleXHTTPDownload(
	w http.ResponseWriter,
	r *http.Request,
	sessionID string,
) {
	if !validXHTTPSessionID(sessionID) {
		http.Error(w, "invalid session", http.StatusBadRequest)
		return
	}

	session, ok := s.getOrCreateXHTTPSession(sessionID)
	if !ok {
		http.Error(w, "too many xhttp sessions", http.StatusServiceUnavailable)
		return
	}
	session.markConnected()
	defer s.dropXHTTPSession(sessionID, session)

	// 客户端断开下载请求时关闭队列，让阻塞中的 parse/relay 退出.
	go func() {
		select {
		case <-r.Context().Done():
			session.close()
		case <-session.closed:
		}
	}()

	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	if flusher != nil {
		flusher.Flush()
	}
	writer := &flushWriter{Writer: w, flusher: flusher}

	// 上传分片是连续字节流，VLESS 头可能跨多个 POST.
	reader := bufio.NewReader(session.queue)
	id, cmd, target, err := parseRequestStream(reader)
	if err != nil {
		log.Printf("xhttp packet-up header %s: %v", sessionID, err)
		return
	}
	if !s.hasUser(id) {
		log.Printf("xhttp packet-up reject %s: unknown UUID", sessionID)
		return
	}

	s.active.Add(1)
	defer s.active.Add(-1)
	log.Printf("xhttp packet-up open %s (cmd=%d) target=%s", sessionID, cmd, target)

	switch cmd {
	case cmdTCP:
		s.relayTCPStream(r.Context(), reader, writer, target)
	case cmdUDP:
		s.relayUDPStream(r.Context(), reader, writer, target)
	default:
		log.Printf("xhttp packet-up unsupported command %d from %s", cmd, sessionID)
	}
}
