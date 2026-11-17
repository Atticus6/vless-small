package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"io"
	"log"
	"net"
	"net/http"
	"time"
)

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

	log.Printf("xhttp open %s (cmd=%d) target=%s", r.RemoteAddr, cmd, target)

	s.active.Add(1)
	defer s.active.Add(-1)

	// HTTP/1.1 下 Go 默认会先消费完请求体再允许写响应；XHTTP stream-one
	// 需要上行和下行同时进行，否则客户端边上传边等响应会互相死锁。
	if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
		log.Printf("xhttp enable full duplex: %v", err)
	}

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
