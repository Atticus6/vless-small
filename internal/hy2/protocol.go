// Package hy2 实现 Hysteria 2 协议的最小服务端子集。
//
// 只覆盖 VLESS 场景需要的部分：HTTP/3 认证、0x401 TCP 数据流、QUIC
// datagram UDP 中继。不包含 Brutal 拥塞控制、Salamander 混淆、masquerade
// 和 ACL；认证成功后告知客户端使用它自己的拥塞控制（Hysteria-CC-RX: auto）。
package hy2

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/apernet/quic-go/quicvarint"
)

const (
	// statusAuthOK 是 Hysteria 认证成功的非标准 HTTP 状态码。
	statusAuthOK = 233

	// 认证请求固定使用 Host "hysteria"、路径 /auth。
	authHost = "hysteria"
	authPath = "/auth"

	headerAuth       = "Hysteria-Auth"
	headerUDPEnabled = "Hysteria-UDP"
	headerCCRX       = "Hysteria-CC-RX"
	headerPadding    = "Hysteria-Padding"

	// frameTypeTCPRequest 是 Hysteria 在 HTTP/3 连接上开启 TCP 数据流时
	// 使用的第一个帧类型。StreamDispatcher 只会 peek 类型，业务代码需要
	// 自己消费掉这个 varint。
	frameTypeTCPRequest = 0x401

	// 与 Hysteria 一致的边界，防止恶意长度导致大分配。
	maxAddressLength     = 2048
	maxMessageLength     = 2048
	maxPaddingLength     = 4096
	maxUDPSize           = 4096
	maxDatagramFrameSize = 1200
)

// randomPadding 生成指定长度的随机可见字符，仅用于掩盖认证报文的长度特征。
func randomPadding(n int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	if n <= 0 {
		return ""
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return ""
	}
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf)
}

// readTCPRequest 读取 TCPRequest 的地址与 padding。
//
// 帧类型 0x401 已由 StreamDispatcher 消费，这里从地址长度开始读：
//
//	[varint] 地址长度
//	[bytes]  地址（host:port）
//	[varint] padding 长度
//	[bytes]  padding
func readTCPRequest(r io.Reader) (string, error) {
	br := quicvarint.NewReader(r)
	addrLen, err := quicvarint.Read(br)
	if err != nil {
		return "", fmt.Errorf("read address length: %w", err)
	}
	if addrLen == 0 || addrLen > maxAddressLength {
		return "", fmt.Errorf("invalid address length %d", addrLen)
	}
	addr := make([]byte, addrLen)
	if _, err := io.ReadFull(r, addr); err != nil {
		return "", fmt.Errorf("read address: %w", err)
	}
	paddingLen, err := quicvarint.Read(br)
	if err != nil {
		return "", fmt.Errorf("read padding length: %w", err)
	}
	if paddingLen > maxPaddingLength {
		return "", fmt.Errorf("invalid padding length %d", paddingLen)
	}
	if paddingLen > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(paddingLen)); err != nil {
			return "", fmt.Errorf("discard padding: %w", err)
		}
	}
	return string(addr), nil
}

// writeTCPResponse 写出 TCPResponse：
//
//	[uint8]  状态（0=OK，1=Error）
//	[varint] 消息长度
//	[bytes]  消息
//	[varint] padding 长度（固定 0）
func writeTCPResponse(w io.Writer, ok bool, message string) error {
	if len(message) > maxMessageLength {
		message = message[:maxMessageLength]
	}
	buf := make([]byte, 0, 1+quicvarint.Len(uint64(len(message)))+len(message)+1)
	if ok {
		buf = append(buf, 0)
	} else {
		buf = append(buf, 1)
	}
	buf = quicvarint.Append(buf, uint64(len(message)))
	buf = append(buf, message...)
	buf = quicvarint.Append(buf, 0)
	_, err := w.Write(buf)
	return err
}

// udpMessage 是 Hysteria 在 QUIC datagram 里承载的 UDP 报文。
type udpMessage struct {
	SessionID uint32
	PacketID  uint16
	FragID    uint8
	FragCount uint8
	Addr      string
	Data      []byte
}

// parseUDPMessage 解析 datagram；格式：
//
//	[uint32] SessionID
//	[uint16] PacketID
//	[uint8]  FragmentID
//	[uint8]  FragmentCount
//	[varint] 地址长度
//	[bytes]  地址
//	[bytes]  payload
func parseUDPMessage(data []byte) (*udpMessage, error) {
	if len(data) < 8 {
		return nil, errors.New("short UDP message")
	}
	msg := &udpMessage{
		SessionID: binary.BigEndian.Uint32(data[0:4]),
		PacketID:  binary.BigEndian.Uint16(data[4:6]),
		FragID:    data[6],
		FragCount: data[7],
	}
	rest := bytes.NewReader(data[8:])
	br := quicvarint.NewReader(rest)
	addrLen, err := quicvarint.Read(br)
	if err != nil {
		return nil, fmt.Errorf("read UDP address length: %w", err)
	}
	if addrLen == 0 || addrLen > maxAddressLength {
		return nil, fmt.Errorf("invalid UDP address length %d", addrLen)
	}
	addr := make([]byte, addrLen)
	if _, err := io.ReadFull(rest, addr); err != nil {
		return nil, fmt.Errorf("read UDP address: %w", err)
	}
	payload, err := io.ReadAll(io.LimitReader(rest, maxUDPSize+1))
	if err != nil {
		return nil, fmt.Errorf("read UDP payload: %w", err)
	}
	if len(payload) > maxUDPSize {
		return nil, fmt.Errorf("UDP payload too large: %d", len(payload))
	}
	msg.Addr = string(addr)
	msg.Data = payload
	return msg, nil
}

// appendTo 把消息编码到 dst，返回新的切片。
func (m *udpMessage) appendTo(dst []byte) []byte {
	dst = binary.BigEndian.AppendUint32(dst, m.SessionID)
	dst = binary.BigEndian.AppendUint16(dst, m.PacketID)
	dst = append(dst, m.FragID, m.FragCount)
	dst = quicvarint.Append(dst, uint64(len(m.Addr)))
	dst = append(dst, m.Addr...)
	return append(dst, m.Data...)
}

// headerSize 返回 UDP 消息头的长度（不含 payload）。
func (m *udpMessage) headerSize() int {
	return 8 + quicvarint.Len(uint64(len(m.Addr))) + len(m.Addr)
}

// fragmentUDPMessage 按最大 datagram 大小把一条 UDP 消息拆成多片。
func fragmentUDPMessage(m *udpMessage, maxSize int) []*udpMessage {
	if m.headerSize()+len(m.Data) <= maxSize {
		return []*udpMessage{m}
	}
	maxPayload := maxSize - m.headerSize()
	if maxPayload <= 0 {
		return nil
	}
	count := (len(m.Data) + maxPayload - 1) / maxPayload
	if count > 255 {
		return nil
	}
	frags := make([]*udpMessage, 0, count)
	for i := 0; i < count; i++ {
		start := i * maxPayload
		end := start + maxPayload
		if end > len(m.Data) {
			end = len(m.Data)
		}
		frag := *m
		frag.FragID = uint8(i)
		frag.FragCount = uint8(count)
		frag.Data = m.Data[start:end]
		frags = append(frags, &frag)
	}
	return frags
}

// defragger 按 PacketID 重组分片。
//
// 与 Hysteria 的实现一致：同一时刻只保留一个未完成的包；新包到达时丢弃
// 之前未收齐的分片。UDP 本身不保证顺序，但 QUIC datagram 在单连接内基本
// 有序，这个简化对代理场景足够。
type defragger struct {
	packetID uint16
	frags    [][]byte
	count    uint8
	size     int
}

func (d *defragger) feed(m *udpMessage) *udpMessage {
	if m.FragCount <= 1 {
		return m
	}
	if m.FragID >= m.FragCount {
		return nil
	}
	if m.PacketID != d.packetID || int(m.FragCount) != len(d.frags) {
		d.packetID = m.PacketID
		d.frags = make([][]byte, m.FragCount)
		d.frags[m.FragID] = append([]byte(nil), m.Data...)
		d.count = 1
		d.size = len(m.Data)
		return nil
	}
	if d.frags[m.FragID] != nil {
		return nil
	}
	d.frags[m.FragID] = append([]byte(nil), m.Data...)
	d.count++
	d.size += len(m.Data)
	if int(d.count) != len(d.frags) {
		return nil
	}
	data := make([]byte, 0, d.size)
	for _, frag := range d.frags {
		data = append(data, frag...)
	}
	out := *m
	out.FragID = 0
	out.FragCount = 1
	out.Data = data
	return &out
}
