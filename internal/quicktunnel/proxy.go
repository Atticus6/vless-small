package quicktunnel

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// 数据流协议常量（与 quick tunnel 边缘一致）。
var dataStreamMagic = [6]byte{0x0A, 0x36, 0xCD, 0x12, 0xA1, 0x3E}

const dataStreamVersion = "01"

const (
	connectionTypeHTTP      = 0
	connectionTypeWebsocket = 1
	connectionTypeTCP       = 2
)

const (
	metadataMethod     = "HttpMethod"
	metadataHost       = "HttpHost"
	metadataHeader     = "HttpHeader:"
	metadataHTTPStatus = "HttpStatus"
)

type metadata struct {
	Key string
	Val string
}

// connectRequest 是边缘在每条数据流开头发送的请求描述。
type connectRequest struct {
	dest    string
	kind    uint16
	method  string
	host    string
	headers http.Header
}

// readDataPreamble 读取并校验 6 字节魔数与版本。
func readDataPreamble(r io.Reader) error {
	var head [8]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return fmt.Errorf("read data stream preamble: %w", err)
	}
	if !bytes.Equal(head[:6], dataStreamMagic[:]) {
		return fmt.Errorf("unexpected data stream magic %x", head[:6])
	}
	if string(head[6:]) != dataStreamVersion {
		return fmt.Errorf("unsupported data stream version %q", head[6:])
	}
	return nil
}

func writeDataPreamble(w io.Writer) error {
	var head [8]byte
	copy(head[:6], dataStreamMagic[:])
	copy(head[6:], dataStreamVersion)
	_, err := w.Write(head[:])
	return err
}

// parseConnectRequest 解析 Cap'n Proto ConnectRequest。
func parseConnectRequest(msg *message) (connectRequest, error) {
	root, err := msg.rootStruct()
	if err != nil {
		return connectRequest{}, err
	}
	dest, err := msg.textAt(root, 0)
	if err != nil {
		return connectRequest{}, err
	}
	if dest == "" {
		return connectRequest{}, fmt.Errorf("empty destination")
	}

	request := connectRequest{
		dest:    dest,
		kind:    msg.u16At(root, 0),
		headers: make(http.Header),
	}

	seg, ptrWord, err := msg.ptrAt(root, 1)
	if err != nil {
		return connectRequest{}, err
	}
	list, err := msg.listAt(seg, ptrWord)
	if err != nil {
		// 没有 metadata 列表也允许：纯 GET 请求可能没有额外头。
		return request, nil
	}
	for i := 0; i < list.elemCount; i++ {
		entry, err := msg.compositeElem(list, i)
		if err != nil {
			return connectRequest{}, err
		}
		key, _ := msg.textAt(entry, 0)
		value, _ := msg.textAt(entry, 1)
		switch key {
		case metadataMethod:
			request.method = value
		case metadataHost:
			request.host = value
		default:
			if strings.HasPrefix(key, metadataHeader) {
				name := strings.TrimPrefix(key, metadataHeader)
				// 这是 h2 模式的内部控制头，不应转发给源站。
				if strings.EqualFold(name, "Cf-Cloudflared-Proxy-Connection-Upgrade") {
					continue
				}
				request.headers.Add(name, value)
			}
		}
	}
	return request, nil
}

// buildConnectRequest 用于测试与协议校验。
func buildConnectRequest(dest string, kind uint16, md []metadata) []byte {
	s, rootData, rootPtrs := newMessage(1, 2)
	s.setU16(rootData, 0, kind)
	s.setText(rootPtrs, dest)
	start := s.setCompositeList(rootPtrs+1, len(md), 0, 2)
	for i, m := range md {
		s.setText(start+i*2, m.Key)
		s.setText(start+i*2+1, m.Val)
	}
	return s.encode()
}

func buildConnectResponse(errText string, md []metadata) []byte {
	s, _, rootPtrs := newMessage(0, 2)
	if errText != "" {
		s.setText(rootPtrs, errText)
	}
	start := s.setCompositeList(rootPtrs+1, len(md), 0, 2)
	for i, m := range md {
		s.setText(start+i*2, m.Key)
		s.setText(start+i*2+1, m.Val)
	}
	return s.encode()
}

// writeConnectResponse 写出带前导的 ConnectResponse。
func writeConnectResponse(w io.Writer, errText string, md []metadata) error {
	if err := writeDataPreamble(w); err != nil {
		return err
	}
	_, err := w.Write(buildConnectResponse(errText, md))
	return err
}

// writeConnectError 在尚未发送响应头时返回错误状态。
func writeConnectError(w io.Writer, status int, message string) error {
	return writeConnectResponse(w, message, []metadata{
		{Key: metadataHTTPStatus, Val: strconv.Itoa(status)},
	})
}

// streamReader 把 QUIC 流包装成请求体。
//
// Close 故意不关闭底层流：同一个流接下来还要写响应，提前 FIN 会让边缘认为
// 响应被截断。读取结束由边缘发送的 EOF 决定。
type streamReader struct {
	r io.Reader
}

func (s *streamReader) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *streamReader) Close() error               { return nil }

// handleDataStream 处理一条边缘发起的 HTTP/WebSocket 数据流。
func (c *Client) handleDataStream(ctx context.Context, rw io.ReadWriteCloser) {
	defer rw.Close()

	br := bufio.NewReaderSize(rw, 16*1024)
	if err := readDataPreamble(br); err != nil {
		c.logf("tunnel: %v", err)
		return
	}
	msg, err := readMessage(br)
	if err != nil {
		c.logf("tunnel: read connect request: %v", err)
		return
	}
	request, err := parseConnectRequest(msg)
	if err != nil {
		_ = writeConnectError(rw, http.StatusBadGateway, err.Error())
		return
	}
	if request.kind == connectionTypeTCP {
		_ = writeConnectError(rw, http.StatusBadGateway, "TCP data streams are not supported")
		return
	}

	if err := c.proxy(ctx, br, rw, request); err != nil {
		c.logf("tunnel: proxy %s %s: %v", request.method, request.dest, err)
	}
}

func (c *Client) proxy(
	ctx context.Context,
	br *bufio.Reader,
	rw io.ReadWriteCloser,
	request connectRequest,
) error {
	method := strings.TrimSpace(request.method)
	if method == "" {
		method = http.MethodGet
	}

	// 边缘下发的是公网请求 URL（例如 https://<quick-tunnel>/ws?x=1）；
	// 与 cloudflared 一样，只保留 path/query，把 scheme/host 改写到本地服务。
	originURL, err := url.Parse(request.dest)
	if err != nil || originURL.Host == "" {
		message := "invalid destination URL"
		_ = writeConnectError(rw, http.StatusBadGateway, message)
		return fmt.Errorf("%s: %q", message, request.dest)
	}
	originURL.Scheme = c.localURL.Scheme
	originURL.Host = c.localURL.Host
	originURL.User = nil
	if originURL.Path == "" {
		originURL.Path = "/"
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, originURL.String(), &streamReader{r: br})
	if err != nil {
		_ = writeConnectError(rw, http.StatusBadGateway, err.Error())
		return err
	}
	if request.host != "" {
		httpReq.Host = request.host
	}
	httpReq.Header = request.headers.Clone()

	// 边缘已经给出 Content-Length 时必须显式设置，否则 Transport 会改用
	// chunked，改变源站看到的请求。
	if raw := httpReq.Header.Get("Content-Length"); raw != "" {
		if length, err := strconv.ParseInt(raw, 10, 64); err == nil {
			httpReq.ContentLength = length
		}
	}
	isChunked := strings.Contains(strings.ToLower(httpReq.Header.Get("Transfer-Encoding")), "chunked")
	if request.kind == connectionTypeWebsocket {
		// 边缘不会转发 Connection/Upgrade 等逐跳头；与 cloudflared 一样由
		// 客户端根据连接类型补齐，否则源站只会收到一个普通 GET。
		httpReq.Header.Set("Connection", "Upgrade")
		httpReq.Header.Set("Upgrade", "websocket")
		if httpReq.Header.Get("Sec-Websocket-Version") == "" {
			httpReq.Header.Set("Sec-Websocket-Version", "13")
		}

		// WebSocket 升级请求没有 HTTP body；101 之后的字节是 WS 帧，必须由
		// 双向复制处理。若让 Transport 读 Body，它会把后续 WS 数据当成
		// chunked 请求体编码，并和复制协程并发读同一个 bufio.Reader。
		httpReq.Body = http.NoBody
		httpReq.ContentLength = 0
		httpReq.Header.Del("Transfer-Encoding")
	} else if !isChunked && httpReq.ContentLength == 0 {
		httpReq.Body = http.NoBody
	}

	resp, err := c.transport.RoundTrip(httpReq)
	if err != nil {
		_ = writeConnectError(rw, http.StatusBadGateway, err.Error())
		return err
	}
	defer resp.Body.Close()

	md := make([]metadata, 0, 2+len(resp.Header))
	md = append(md, metadata{Key: metadataHTTPStatus, Val: strconv.Itoa(resp.StatusCode)})
	for name, values := range resp.Header {
		for _, value := range values {
			md = append(md, metadata{Key: metadataHeader + name, Val: value})
		}
	}
	if err := writeConnectResponse(rw, "", md); err != nil {
		return err
	}

	// 101 只可能来自协议升级；无论边缘是否把 type 标成 websocket，都应
	// 切换为双向字节通道。
	if resp.StatusCode == http.StatusSwitchingProtocols {
		origin, ok := resp.Body.(io.ReadWriteCloser)
		if !ok {
			return fmt.Errorf("origin did not provide a bidirectional body")
		}
		done := make(chan struct{}, 2)
		go func() {
			_, _ = io.Copy(origin, br)
			_ = origin.Close()
			done <- struct{}{}
		}()
		go func() {
			_, _ = io.Copy(rw, origin)
			_ = rw.Close()
			done <- struct{}{}
		}()
		select {
		case <-ctx.Done():
		case <-done:
		}
		_ = origin.Close()
		_ = rw.Close()
		return nil
	}

	if _, err := io.Copy(rw, resp.Body); err != nil {
		return fmt.Errorf("copy response body: %w", err)
	}
	return rw.Close()
}
