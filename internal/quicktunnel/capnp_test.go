package quicktunnel

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestConnectRequestRoundTrip(t *testing.T) {
	frame := buildConnectRequest(
		"http://127.0.0.1:8080/ws",
		connectionTypeWebsocket,
		[]metadata{
			{Key: metadataMethod, Val: "GET"},
			{Key: metadataHost, Val: "demo.trycloudflare.com"},
			{Key: metadataHeader + "Upgrade", Val: "websocket"},
			{Key: metadataHeader + "Cf-Cloudflared-Proxy-Connection-Upgrade", Val: "control-stream"},
		},
	)

	msg, err := readMessage(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	request, err := parseConnectRequest(msg)
	if err != nil {
		t.Fatalf("parseConnectRequest: %v", err)
	}

	if request.dest != "http://127.0.0.1:8080/ws" {
		t.Fatalf("dest = %q", request.dest)
	}
	if request.kind != connectionTypeWebsocket {
		t.Fatalf("type = %d", request.kind)
	}
	if request.method != "GET" || request.host != "demo.trycloudflare.com" {
		t.Fatalf("method=%q host=%q", request.method, request.host)
	}
	if got := request.headers.Get("Upgrade"); got != "websocket" {
		t.Fatalf("upgrade = %q", got)
	}
	// h2 内部控制头必须被丢弃，不能泄漏给源站。
	if got := request.headers.Get("Cf-Cloudflared-Proxy-Connection-Upgrade"); got != "" {
		t.Fatalf("internal header leaked: %q", got)
	}
}

func TestConnectResponseRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := writeConnectResponse(&buf, "boom", []metadata{
		{Key: metadataHTTPStatus, Val: "502"},
		{Key: metadataHeader + "X-Test", Val: "one"},
	}); err != nil {
		t.Fatalf("writeConnectResponse: %v", err)
	}

	if err := readDataPreamble(&buf); err != nil {
		t.Fatalf("preamble: %v", err)
	}
	msg, err := readMessage(&buf)
	if err != nil {
		t.Fatalf("readMessage: %v", err)
	}
	root, err := msg.rootStruct()
	if err != nil {
		t.Fatal(err)
	}
	errText, _ := msg.textAt(root, 0)
	if errText != "boom" {
		t.Fatalf("error = %q", errText)
	}
	seg, ptrWord, err := msg.ptrAt(root, 1)
	if err != nil {
		t.Fatal(err)
	}
	list, err := msg.listAt(seg, ptrWord)
	if err != nil {
		t.Fatal(err)
	}
	if list.elemCount != 2 {
		t.Fatalf("metadata count = %d", list.elemCount)
	}
	first, err := msg.compositeElem(list, 0)
	if err != nil {
		t.Fatal(err)
	}
	key, _ := msg.textAt(first, 0)
	val, _ := msg.textAt(first, 1)
	if key != metadataHTTPStatus || val != "502" {
		t.Fatalf("metadata[0] = %s=%s", key, val)
	}
}

func TestRPCMessageLayouts(t *testing.T) {
	t.Run("bootstrap", func(t *testing.T) {
		msg, err := readMessage(bytes.NewReader(buildBootstrap(0)))
		if err != nil {
			t.Fatal(err)
		}
		root, err := msg.rootStruct()
		if err != nil {
			t.Fatal(err)
		}
		if tag := msg.u16At(root, 0); tag != messageBootstrap {
			t.Fatalf("tag = %d", tag)
		}
		seg, ptrWord, err := msg.ptrAt(root, 0)
		if err != nil {
			t.Fatal(err)
		}
		bootstrap, err := msg.structAt(seg, ptrWord)
		if err != nil {
			t.Fatal(err)
		}
		if qid := msg.u32At(bootstrap, 0); qid != 0 {
			t.Fatalf("question id = %d", qid)
		}
	})

	t.Run("finish keeps result caps", func(t *testing.T) {
		msg, err := readMessage(bytes.NewReader(buildFinish(7, false)))
		if err != nil {
			t.Fatal(err)
		}
		root, err := msg.rootStruct()
		if err != nil {
			t.Fatal(err)
		}
		if tag := msg.u16At(root, 0); tag != messageFinish {
			t.Fatalf("tag = %d", tag)
		}
		seg, ptrWord, _ := msg.ptrAt(root, 0)
		finish, err := msg.structAt(seg, ptrWord)
		if err != nil {
			t.Fatal(err)
		}
		if qid := msg.u32At(finish, 0); qid != 7 {
			t.Fatalf("question id = %d", qid)
		}
		// rpc.capnp 的 getter 取反此位：bit32=1 => releaseResultCaps=false。
		if !msg.boolAt(finish, 32) {
			t.Fatal("releaseResultCaps should be false")
		}
	})

	t.Run("return exception", func(t *testing.T) {
		msg, err := readMessage(bytes.NewReader(buildReturnException(3, "nope")))
		if err != nil {
			t.Fatal(err)
		}
		root, err := msg.rootStruct()
		if err != nil {
			t.Fatal(err)
		}
		if tag := msg.u16At(root, 0); tag != messageReturn {
			t.Fatalf("tag = %d", tag)
		}
		seg, ptrWord, _ := msg.ptrAt(root, 0)
		ret, err := msg.structAt(seg, ptrWord)
		if err != nil {
			t.Fatal(err)
		}
		if which := msg.u16At(ret, 6); which != 1 {
			t.Fatalf("return arm = %d", which)
		}
		excSeg, excPtr, _ := msg.ptrAt(ret, 0)
		exc, err := msg.structAt(excSeg, excPtr)
		if err != nil {
			t.Fatal(err)
		}
		reason, _ := msg.textAt(exc, 0)
		if reason != "nope" {
			t.Fatalf("reason = %q", reason)
		}
	})
}

func TestDecodeTunnelID(t *testing.T) {
	id, err := decodeTunnelID("14725836-1234-5678-9abc-def012345678")
	if err != nil {
		t.Fatalf("decodeTunnelID: %v", err)
	}
	if len(id) != 16 {
		t.Fatalf("len = %d", len(id))
	}

	if _, err := decodeTunnelID("not-a-uuid"); err == nil {
		t.Fatal("expected error for invalid UUID")
	}
}

func TestBuildLocalConfig(t *testing.T) {
	raw := buildLocalConfig("http://127.0.0.1:8080")
	msg := string(raw)
	if !bytes.Contains(raw, []byte(`"service":"http://127.0.0.1:8080"`)) {
		t.Fatalf("missing service: %s", msg)
	}
	if !bytes.Contains(raw, []byte(`"ingress"`)) || !bytes.Contains(raw, []byte(`"warp-routing"`)) {
		t.Fatalf("unexpected config: %s", msg)
	}
}

// TestReadMessageRejectsOversizedTotal 验证多段消息按总字节数限流：
// 单段都不算大，但总和超过上限时必须在分配前拒绝。这个检查同时避免了
// 32 位平台上 int(size)*8 溢出绕过上限。
func TestReadMessageRejectsOversizedTotal(t *testing.T) {
	buf := make([]byte, 16)
	binary.LittleEndian.PutUint32(buf[0:], 1)     // 段数 - 1 = 1，即两段
	binary.LittleEndian.PutUint32(buf[4:], 1<<20) // 第一段 1M words
	binary.LittleEndian.PutUint32(buf[8:], 1<<20) // 第二段 1M words
	// 段表占 12 字节（4 + 4*2），补齐到 16 字节后才会进入段数据；
	// 总长度检查在读取段数据之前触发。

	if _, err := readMessage(bytes.NewReader(buf)); err == nil {
		t.Fatal("expected message size limit error")
	}
}

// TestReadMessageRejectsTooManySegments 覆盖段数上限。
func TestReadMessageRejectsTooManySegments(t *testing.T) {
	buf := make([]byte, 4)
	binary.LittleEndian.PutUint32(buf, maxCapnpSegments)
	if _, err := readMessage(bytes.NewReader(buf)); err == nil {
		t.Fatal("expected segment count error")
	}
}

// TestPointerFollowsFarPointer 验证 far 指针的偏移从 bit 3 开始读取。
//
// far pointer 格式：bits 0-1 = 类型 2，bit 2 = landing pad 大小，
// bits 3-31 = landing pad 的 word 偏移，bits 32-63 = 段号。
func TestPointerFollowsFarPointer(t *testing.T) {
	const offset = 2
	far := uint64(2) | (offset << 3) | (uint64(1) << 32)
	msg := &message{segments: [][]uint64{
		{far},
		{0, 0, 0, 0},
	}}

	kind, seg, ptr, err := msg.pointer(0, 0)
	if err != nil {
		t.Fatalf("pointer: %v", err)
	}
	if kind != 0 || seg != 1 || ptr != offset {
		t.Fatalf(
			"pointer = kind %d seg %d word %d, want struct seg 1 word %d",
			kind, seg, ptr, offset,
		)
	}
}
