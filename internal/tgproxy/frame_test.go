package tgproxy

import (
	"bytes"
	"testing"
)

func TestEncodeSplitFrame(t *testing.T) {
	enc, err := encodeFrame(frameData, 0x123456, []byte{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(enc, []byte{0x02, 0x12, 0x34, 0x56, 0, 0, 0, 3, 1, 2, 3}) {
		t.Fatalf("encode=%x", enc)
	}
	// 一帧 + 一帧拼在一起
	two := append(append([]byte(nil), enc...), mustFrame(t, frameClose, 1, nil)...)
	frames, err := splitFrames(two)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames", len(frames))
	}
	if frames[0].Type != frameData || frames[0].SID != 0x123456 || !bytes.Equal(frames[0].Payload, []byte{1, 2, 3}) {
		t.Fatalf("frame0=%+v", frames[0])
	}
	if frames[1].Type != frameClose || frames[1].SID != 1 {
		t.Fatalf("frame1=%+v", frames[1])
	}
}

func mustFrame(t *testing.T, typ byte, sid uint32, data []byte) []byte {
	t.Helper()
	b, err := encodeFrame(typ, sid, data)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestSplitFramesRejectsBad(t *testing.T) {
	cases := [][]byte{
		{0x02, 0, 0, 1, 0, 0, 0, 5, 1, 2}, // payload 长度不足
		{0x7f, 0, 0, 1, 0, 0, 0, 0},       // 非法类型
		{0x02, 0, 0, 1, 0, 0, 0, 0},       // data 帧空 payload
		{0x04, 0, 0, 1, 0, 0, 0, 2, 1, 2}, // window 帧不是 4 字节
	}
	for i, c := range cases {
		if _, err := splitFrames(c); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestPktSplitterEF(t *testing.T) {
	// mode ef：1 字节词头，words=3 → payload 12 字节
	pkt := append([]byte{3}, bytes.Repeat([]byte{0xaa}, 12)...)
	p := newPktSplitter(0xef)
	// 分两次喂，验证粘包重组
	out, err := p.push(pkt[:5])
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("early pkt %d", len(out))
	}
	out, err = p.push(pkt[5:])
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !bytes.Equal(out[0], pkt) {
		t.Fatalf("got %d pkts", len(out))
	}
}

func TestPktSplitterEFExt(t *testing.T) {
	// words=0x7f 用 3 字节小端扩展：words=130 → 520 字节 payload
	pkt := make([]byte, 4+520)
	pkt[0] = 0x7f
	n := 130
	pkt[1] = byte(n)
	pkt[2] = byte(n >> 8)
	pkt[3] = byte(n >> 16)
	for i := 4; i < len(pkt); i++ {
		pkt[i] = byte(i)
	}
	p := newPktSplitter(0xef)
	out, err := p.push(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !bytes.Equal(out[0], pkt) {
		t.Fatalf("ext got %d", len(out))
	}
}

func TestPktSplitterDD(t *testing.T) {
	// mode dd：4 字节小端长度 5
	pkt := append([]byte{5, 0, 0, 0}, []byte{1, 2, 3, 4, 5}...)
	p := newPktSplitter(0xdd)
	out, err := p.push(pkt)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || !bytes.Equal(out[0], pkt) {
		t.Fatalf("dd got %d", len(out))
	}
	// 两个连续包
	out, err = p.push(append(append([]byte(nil), pkt...), pkt...))
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("dd two got %d", len(out))
	}
}
