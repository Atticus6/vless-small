package tgproxy

import (
	"encoding/binary"
	"errors"
)

// lane 帧类型：type|sid(3B)|len(4B)|payload。
const (
	frameOpen    = 0x01 // 打开一条到 TG DC 的流（payload 为空）
	frameData    = 0x02 // 数据（payload 为密文字节）
	frameClose   = 0x03 // 关闭（payload 为空）
	frameWindow  = 0x04 // 窗口增量（payload 为 4B BE 增量）
	frameWelcome = 0x11 // 服务端欢迎帧（session 创建响应）
)

const (
	maxFrameLen = 1024 * 1024
	maxSID      = 0xffffff
)

func isFrameType(t byte) bool {
	return t == frameOpen || t == frameData || t == frameClose || t == frameWindow || t == frameWelcome
}

// Frame 是一条 lane 帧。
type Frame struct {
	Type    byte
	SID     uint32
	Payload []byte
}

// encodeFrame 把帧编码成线格式。
func encodeFrame(t byte, sid uint32, data []byte) ([]byte, error) {
	if !isFrameType(t) || sid > maxSID || len(data) > maxFrameLen {
		return nil, errors.New("bad frame")
	}
	out := make([]byte, 8+len(data))
	out[0] = t
	out[1] = byte(sid >> 16)
	out[2] = byte(sid >> 8)
	out[3] = byte(sid)
	binary.BigEndian.PutUint32(out[4:8], uint32(len(data)))
	copy(out[8:], data)
	return out, nil
}

// splitFrames 把一个 WebSocket 消息切成若干帧（一条消息可含多帧）。
func splitFrames(b []byte) ([]Frame, error) {
	var out []Frame
	for o := 0; o < len(b); {
		if len(b)-o < 8 {
			return nil, errors.New("short frame")
		}
		t := b[o]
		if !isFrameType(t) {
			return nil, errors.New("bad frame type")
		}
		sid := uint32(b[o+1])<<16 | uint32(b[o+2])<<8 | uint32(b[o+3])
		n := binary.BigEndian.Uint32(b[o+4 : o+8])
		if n > maxFrameLen {
			return nil, errors.New("frame too large")
		}
		total := 8 + int(n)
		if len(b)-o < total {
			return nil, errors.New("short frame payload")
		}
		// 校验 payload 形态，data 帧 payload 不能为空。
		payload := b[o+8 : o+total]
		switch t {
		case frameOpen, frameClose, frameWelcome:
			if len(payload) != 0 {
				return nil, errors.New("unexpected payload")
			}
		case frameData:
			if len(payload) == 0 {
				return nil, errors.New("empty data frame")
			}
		case frameWindow:
			if len(payload) != 4 {
				return nil, errors.New("bad window frame")
			}
		}
		out = append(out, Frame{Type: t, SID: sid, Payload: payload})
		o += total
		if len(out) > 4096 {
			return nil, errors.New("too many frames")
		}
	}
	return out, nil
}

// windowDelta 读取 window 帧的 4B BE 增量。
func windowDelta(payload []byte) (uint32, error) {
	if len(payload) != 4 {
		return 0, errors.New("bad window")
	}
	n := binary.BigEndian.Uint32(payload)
	if n == 0 {
		return 0, errors.New("zero window")
	}
	return n, nil
}

// pktSplitter 按 TG mode 对下行字节流切包。
//
// mode 0xef：包长是 1/4 字节“词数”（words*4）；0x7f 表示用后续 3 字节小端
// 表示词数。mode 0xdd/0xee：4 字节小端长度（去掉最高位）。
type pktSplitter struct {
	tail []byte
	mode byte
}

func newPktSplitter(mode byte) *pktSplitter {
	return &pktSplitter{mode: mode}
}

// push 追加数据，返回所有完整的 TG 包（含长度头）。
func (p *pktSplitter) push(v []byte) ([][]byte, error) {
	if len(v) > 0 {
		p.tail = append(p.tail, v...)
	}
	var pkts [][]byte
	o := 0
	for {
		avail := len(p.tail) - o
		var hLen int
		var pLen uint32
		if p.mode == 0xef {
			if avail < 1 {
				break
			}
			words := uint32(p.tail[o] & 0x7f)
			if words == 0x7f {
				if avail < 4 {
					break
				}
				words = uint32(p.tail[o+1]) | uint32(p.tail[o+2])<<8 | uint32(p.tail[o+3])<<16
				hLen = 4
			} else {
				hLen = 1
			}
			pLen = words * 4
		} else {
			if avail < 4 {
				break
			}
			pLen = binary.LittleEndian.Uint32(p.tail[o:o+4]) & 0x7fffffff
			hLen = 4
		}
		if pLen == 0 || pLen > 8*1024*1024 {
			return nil, errors.New("bad TG packet length")
		}
		total := hLen + int(pLen)
		if avail < total {
			break
		}
		pkts = append(pkts, append([]byte(nil), p.tail[o:o+total]...))
		o += total
	}
	if o > 0 {
		p.tail = append([]byte(nil), p.tail[o:]...)
	}
	if len(p.tail) > 8*1024*1024+4 {
		return nil, errors.New("TG packet tail too large")
	}
	return pkts, nil
}
