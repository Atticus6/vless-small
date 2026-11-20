package tgproxy

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// TG WebK 的 DC 域名模板。dc 从 1 起；media 流用 -1 后缀。
func upstreamHosts(dc int, media bool) []string {
	suffix, alt := "", "-1"
	if media {
		suffix, alt = "-1", ""
	}
	return []string{
		"kws" + itoa(dc) + suffix + ".web." + "telegram" + ".org",
		"kws" + itoa(dc) + alt + ".web." + "telegram" + ".org",
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// upPack 是上行合并阈值（与 JS CFG.upPack 一致）。
const upPack = 20 * 1024

// tgStream 对应 JS 的 makeStream：一条到 TG DC 的加密流。
//
// 生命周期：
//   - push(客户端密文) 首个 64 字节解密伪装头 → 连 TG WebK → 之后双向转发
//   - emit 回调把解密后的下行字节交回 lane（由 lane 层封成 data 帧）
type tgStream struct {
	skey []byte

	mu         sync.Mutex
	hdr        []byte        // 尚未凑够 64 字节的伪装头
	cIn        *streamCipher // 客户端→TG：先解密客户端外层
	cOut       *streamCipher // TG→客户端：外层加密回客户端
	kIn        *streamCipher // TG 内层数据解密
	kOut       *streamCipher // 发往 TG 的内层加密
	upSplitter *pktSplitter  // 上行：把客户端字节流切成 TG 包（带状态，处理粘包）
	ws         *websocket.Conn
	init       bool
	closed     bool
	mode       byte

	emit    func([]byte) error
	onClose func()

	writeMu sync.Mutex // 串行化写 WebK
}

func newTGStream(skey []byte, emit func([]byte) error, onClose func()) *tgStream {
	return &tgStream{skey: skey, emit: emit, onClose: onClose}
}

// push 接收客户端发来的密文字节（可能是伪装头的一部分，也可能是数据）。
func (s *tgStream) push(data []byte) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return errors.New("closed")
	}
	if s.ws == nil {
		// 还在收集 64 字节伪装头。
		need := 64 - len(s.hdr)
		if len(data) < need {
			s.hdr = append(s.hdr, data...)
			s.mu.Unlock()
			return nil
		}
		s.hdr = append(s.hdr, data[:need]...)
		rest := data[need:]
		head := s.hdr
		s.mu.Unlock()
		if err := s.start(head); err != nil {
			return err
		}
		if len(rest) > 0 {
			return s.write(rest)
		}
		return nil
	}
	s.mu.Unlock()
	return s.write(data)
}

// start 解密伪装头、连 TG、完成握手。
func (s *tgStream) start(head []byte) error {
	// 四个 cipher 的方向必须与 JS 参考 makeStream.start() 逐字节一致，
	// 否则 HTTP 信令能通、但真正转发 TG 流量时加解密对不上。
	//
	//   cr  = cipher(raw, skey, false)   // 解密客户端外层
	//   cw  = cipher(raw, skey, true)    // 加密回客户端外层
	//   out = headWire(newHead)          // 发往 TG 的新头（cipher 已消费整头）
	//   kr  = cipher(newHead, nil, true) // 解密 TG 内层数据
	//
	// 1. 解密客户端伪装头，读出 mode/dc/media。reverse=false。
	cIn, err := cipherFor(head, s.skey, false)
	if err != nil {
		return err
	}
	plain := cIn.crypt(append([]byte(nil), head...))
	mode, target, media, err := parseHead(plain)
	if err != nil {
		return err
	}

	// 2. 生成新的随机伪装头（给 TG）。
	newHead, err := makeRandomHead(mode, target, media)
	if err != nil {
		return err
	}
	// 3. 发往 TG 的线格式：前 56 字节明文，后 8 字节加密。
	//    headWire 返回的 cipher 已消费整个 newHead，必须复用它作为 kOut。
	wireHead, kOut, err := makeHeadWire(newHead)
	if err != nil {
		return err
	}
	// 4. 解密 TG 内层数据：newHead 作 key（无 skey），reverse=true。
	kIn, err := cipherFor(newHead, nil, true)
	if err != nil {
		return err
	}
	// 5. 加密回客户端的外层：用原始 head + skey，reverse=true。
	cOut, err := cipherFor(head, s.skey, true)
	if err != nil {
		return err
	}

	// 7. 连 TG WebK。
	ws, err := dialUpstream(target+1, media)
	if err != nil {
		return err
	}

	s.mu.Lock()
	s.ws = ws
	s.cIn = cIn
	s.cOut = cOut
	s.kIn = kIn
	s.kOut = kOut
	s.upSplitter = newPktSplitter(mode)
	s.mode = mode
	s.init = true
	s.mu.Unlock()

	// 8. 先发伪装头，再启动读循环。
	if err := s.writeRaw(wireHead); err != nil {
		s.close()
		return err
	}
	go s.readLoop()
	return nil
}

// write 处理客户端数据：解密外层 → TG 分包 → 内层加密 → 合并写 WebK。
func (s *tgStream) write(raw []byte) error {
	s.mu.Lock()
	if s.closed || !s.init {
		s.mu.Unlock()
		return errors.New("not ready")
	}
	cIn := s.cIn
	kOut := s.kOut
	upSplitter := s.upSplitter
	s.mu.Unlock()

	// 客户端数据解密外层后是 TG 包流，用带状态的 splitter 切包（处理跨 WebSocket
	// 消息粘包），每个包单独内层加密后发往 WebK。
	plain := cIn.crypt(append([]byte(nil), raw...))
	s.mu.Lock()
	pkts, err := upSplitter.push(plain)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, pkt := range pkts {
		wire := kOut.crypt(pkt)
		if err := s.writeRaw(wire); err != nil {
			return err
		}
	}
	return nil
}

// writeRaw 串行写 WebK。
func (s *tgStream) writeRaw(b []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	ws := s.ws
	closed := s.closed
	s.mu.Unlock()
	if closed || ws == nil {
		return errors.New("closed")
	}
	return ws.WriteMessage(websocket.BinaryMessage, b)
}

// readLoop 从 WebK 读数据：内层解密 → 外层加密 → emit 回 lane。
func (s *tgStream) readLoop() {
	defer s.close()
	for {
		s.mu.Lock()
		ws, kIn, cOut := s.ws, s.kIn, s.cOut
		s.mu.Unlock()
		if ws == nil {
			return
		}
		_, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		if len(msg) == 0 {
			continue
		}
		// TG WebK 的每条消息就是一个完整的 TG 包：内层解密 → 外层加密 → 直接
		// 回给 lane（JS 的 pump 不做切包，readPack 已经按 dnPack 聚合/拆分）。
		plain := kIn.crypt(append([]byte(nil), msg...))
		if len(plain) == 0 {
			continue
		}
		out := cOut.crypt(plain)
		if err := s.emit(out); err != nil {
			return
		}
	}
}

func (s *tgStream) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	ws := s.ws
	s.ws = nil
	s.mu.Unlock()
	if ws != nil {
		_ = ws.Close()
	}
	if s.onClose != nil {
		s.onClose()
	}
}

// dialUpstream 连到 TG WebK，依次尝试两个 DC 域名。
func dialUpstream(dc int, media bool) (*websocket.Conn, error) {
	var lastErr error
	d := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		Subprotocols:     []string{"binary"},
	}
	h := http.Header{}
	for _, host := range upstreamHosts(dc, media) {
		url := "wss://" + host + "/apiws"
		ws, _, err := d.Dial(url, h)
		if err == nil {
			return ws, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream")
	}
	return nil, lastErr
}

// makeRandomHead 生成合法随机伪装头：随机填充，避开保留前缀，填入 mode/dc。
func makeRandomHead(mode byte, target int, media bool) ([]byte, error) {
	head := make([]byte, 64)
	bad := map[string]bool{"HEAD": true, "POST": true, "GET ": true, "OPTI": true}
	for {
		if _, err := rand.Read(head); err != nil {
			return nil, err
		}
		if head[0] == 0xef || bad[string(head[:4])] || allZero(head[4:8]) {
			continue
		}
		break
	}
	for i := 56; i < 60; i++ {
		head[i] = mode
	}
	dc := target + 1
	if media {
		dc = -dc
	}
	binary.LittleEndian.PutUint16(head[60:62], uint16(int16(dc)))
	return head, nil
}

func allZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
