package quicktunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Cap'n Proto RPC 的 Message union tag（rpc.capnp）。
const (
	messageBootstrap = 8
	messageCall      = 2
	messageReturn    = 3
	messageFinish    = 4
)

// RegistrationServer 接口与方法的 ID。
const (
	registrationInterfaceID = 0xf71695ec7fe85497
	methodRegister          = 0
	methodUnregister        = 1
	methodUpdateLocalConfig = 2
)

const rpcTimeout = 10 * time.Second

// newMessage 分配一个根结构体并返回（数据段起点, 指针段起点）。
func newMessage(dataWords, ptrWords int) (*segment, int, int) {
	s := newSegment()
	data := s.alloc(dataWords + ptrWords)
	// words[0] 指向紧邻的根结构体：偏移 0。
	s.words[0] = structPointer(0, dataWords, ptrWords)
	return s, data, data + dataWords
}

// buildBootstrap 构造 Cap'n Proto RPC 的 bootstrap 消息。
func buildBootstrap(questionID uint32) []byte {
	s, rootData, rootPtrs := newMessage(1, 1)
	s.setU16(rootData, 0, messageBootstrap)
	bootstrap := s.alloc(1 + 1)
	s.setStructPtr(rootPtrs, bootstrap, 1, 1)
	s.setU32(bootstrap, 0, questionID)
	return s.encode()
}

// buildCall 构造一次方法调用。
//
// paramsDataWords/paramsPtrWords 是参数结构体的大小；fill 负责填充参数。
// 目标固定为 bootstrap 返回的注册接口（importedCap(0)）。
func buildCall(
	questionID uint32,
	methodID uint16,
	paramsDataWords, paramsPtrWords int,
	fill func(s *segment, dataStart, ptrStart int),
) []byte {
	s, rootData, rootPtrs := newMessage(1, 1)
	s.setU16(rootData, 0, messageCall)

	call := s.alloc(3 + 3)
	s.setStructPtr(rootPtrs, call, 3, 3)
	s.setU32(call, 0, questionID)
	s.setU16(call, 4, methodID)
	s.setU16(call, 6, 0) // sendResultsTo = caller
	s.setU64(call, 8, registrationInterfaceID)

	target := s.alloc(1 + 1)
	s.setStructPtr(call+3, target, 1, 1)
	s.setU32(target, 0, 0) // importedCap(0)
	s.setU16(target, 4, 0) // union: importedCap

	payload := s.alloc(0 + 2)
	s.setStructPtr(call+3+1, payload, 0, 2)

	params := s.alloc(paramsDataWords + paramsPtrWords)
	s.setStructPtr(payload, params, paramsDataWords, paramsPtrWords)
	fill(s, params, params+paramsDataWords)
	return s.encode()
}

// buildFinish 构造 Finish 消息。
//
// releaseResultCaps=false 时保留结果里的 capability（例如 bootstrap 的注册
// 接口）；异常返回时应释放，对应调用方传 true。
func buildFinish(questionID uint32, releaseResultCaps bool) []byte {
	s, rootData, rootPtrs := newMessage(1, 1)
	s.setU16(rootData, 0, messageFinish)
	finish := s.alloc(1)
	s.setStructPtr(rootPtrs, finish, 1, 0)
	s.setU32(finish, 0, questionID)
	// rpc.capnp 的 getter 会取反这一位：bit=1 表示 releaseResultCaps=false。
	s.setBit(finish, 32, !releaseResultCaps)
	return s.encode()
}

// buildReturnException 用于回复边缘发起的未知调用，避免控制流挂住。
func buildReturnException(answerID uint32, reason string) []byte {
	s, rootData, rootPtrs := newMessage(1, 1)
	s.setU16(rootData, 0, messageReturn)
	ret := s.alloc(2 + 1)
	s.setStructPtr(rootPtrs, ret, 2, 1)
	s.setU32(ret, 0, answerID)
	s.setU16(ret, 6, 1) // union: exception
	exc := s.alloc(1 + 1)
	s.setStructPtr(ret+2, exc, 1, 1)
	s.setText(exc+1, reason)
	return s.encode()
}

// rpcReturn 保存一次调用的 Return 结构。
type rpcReturn struct {
	msg *message
	ret structRef
}

// payloadContent 返回结果 Payload 的 content 结构体。
func (r *rpcReturn) payloadContent() (structRef, error) {
	payload, err := r.msg.structAt(r.ret.seg, r.ret.data+r.ret.dataWords)
	if err != nil {
		return structRef{}, err
	}
	seg, ptrWord, err := r.msg.ptrAt(payload, 0)
	if err != nil {
		return structRef{}, err
	}
	return r.msg.structAt(seg, ptrWord)
}

// controlStream 封装控制流的 Cap'n Proto RPC 读写。
//
// 控制流是 QUIC 的第一条双向流；bootstrap/call/finish 都按单段消息顺序写入。
// rw 同时实现 SetReadDeadline/SetWriteDeadline 时会给 RPC 调用加超时。
type controlStream struct {
	rw      io.ReadWriter
	br      *bufio.Reader
	writeMu sync.Mutex
	nextID  uint32
}

func newControlStream(rw io.ReadWriter) *controlStream {
	return &controlStream{
		rw: rw,
		br: bufio.NewReaderSize(rw, 4096),
	}
}

func (c *controlStream) setDeadline(timeout time.Duration) {
	type deadliner interface {
		SetReadDeadline(time.Time) error
		SetWriteDeadline(time.Time) error
	}
	if d, ok := c.rw.(deadliner); ok {
		_ = d.SetReadDeadline(time.Now().Add(timeout))
		_ = d.SetWriteDeadline(time.Now().Add(timeout))
	}
}

func (c *controlStream) clearDeadline() {
	if d, ok := c.rw.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = d.SetReadDeadline(time.Time{})
	}
	if d, ok := c.rw.(interface{ SetWriteDeadline(time.Time) error }); ok {
		_ = d.SetWriteDeadline(time.Time{})
	}
}

func (c *controlStream) send(frame []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_, err := c.rw.Write(frame)
	return err
}

// waitReturn 读取消息直到拿到指定 questionId 的 Return。
//
// 边缘可能穿插发送其它消息（例如服务端发起的调用），这里对调用回一个
// 异常，其它消息直接忽略，避免控制流读循环阻塞。
func (c *controlStream) waitReturn(questionID uint32) (*rpcReturn, error) {
	for {
		msg, err := readMessage(c.br)
		if err != nil {
			return nil, fmt.Errorf("read control stream: %w", err)
		}
		root, err := msg.rootStruct()
		if err != nil {
			return nil, err
		}

		switch msg.u16At(root, 0) {
		case messageReturn:
			seg, ptrWord, err := msg.ptrAt(root, 0)
			if err != nil {
				return nil, err
			}
			ret, err := msg.structAt(seg, ptrWord)
			if err != nil {
				return nil, err
			}
			if msg.u32At(ret, 0) != questionID {
				// 迟到的其它调用结果；忽略即可。
				continue
			}
			switch msg.u16At(ret, 6) {
			case 0: // results
				return &rpcReturn{msg: msg, ret: ret}, nil
			case 1: // exception
				excSeg, excPtr, err := msg.ptrAt(ret, 0)
				if err != nil {
					return nil, err
				}
				exc, err := msg.structAt(excSeg, excPtr)
				if err != nil {
					return nil, err
				}
				reason, _ := msg.textAt(exc, 0)
				if reason == "" {
					reason = "unknown capnp exception"
				}
				return nil, fmt.Errorf("control RPC failed: %s", reason)
			default:
				return nil, fmt.Errorf("control RPC returned unsupported arm %d", msg.u16At(ret, 6))
			}
		case messageCall:
			seg, ptrWord, err := msg.ptrAt(root, 0)
			if err != nil {
				return nil, err
			}
			call, err := msg.structAt(seg, ptrWord)
			if err != nil {
				return nil, err
			}
			_ = c.send(buildReturnException(msg.u32At(call, 0), "unsupported server call"))
		default:
			// finish/release 等控制消息与注册流程无关。
		}
	}
}

// call 执行一次 RPC 调用并处理 Finish。
func (c *controlStream) call(
	ctx context.Context,
	methodID uint16,
	paramsDataWords, paramsPtrWords int,
	fill func(s *segment, dataStart, ptrStart int),
) (*rpcReturn, error) {
	c.nextID++
	questionID := c.nextID

	c.setDeadline(rpcTimeout)
	defer c.clearDeadline()

	if err := c.send(buildCall(questionID, methodID, paramsDataWords, paramsPtrWords, fill)); err != nil {
		return nil, fmt.Errorf("write control RPC: %w", err)
	}
	ret, err := c.waitReturn(questionID)
	if err != nil {
		_ = c.send(buildFinish(questionID, true))
		return nil, err
	}
	if err := c.send(buildFinish(questionID, false)); err != nil {
		return nil, fmt.Errorf("write control finish: %w", err)
	}
	return ret, nil
}

// bootstrap 完成 RPC 引导，拿到后续调用的注册接口。
func (c *controlStream) bootstrap(ctx context.Context) error {
	c.nextID = 0
	c.setDeadline(rpcTimeout)
	defer c.clearDeadline()

	if err := c.send(buildBootstrap(0)); err != nil {
		return fmt.Errorf("write bootstrap: %w", err)
	}
	// bootstrap 的 Return 携带 capability，不解析具体内容；失败会以异常返回。
	_, err := c.waitReturn(0)
	return err
}

// connectionDetails 是 registerConnection 成功时的返回。
type connectionDetails struct {
	UUID            []byte
	Location        string
	RemotelyManaged bool
}

// registrationError 表示边缘拒绝注册；ShouldRetry=false 时不应继续重试。
type registrationError struct {
	Cause       string
	RetryAfter  time.Duration
	ShouldRetry bool
}

func (e *registrationError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("registration rejected: %s (retry after %s)", e.Cause, e.RetryAfter)
	}
	return "registration rejected: " + e.Cause
}

// register 在一次 QUIC 连接上完成 registerConnection 调用。
func (c *controlStream) register(
	ctx context.Context,
	creds *Credentials,
	version string,
) (*connectionDetails, error) {
	tunnelID, err := decodeTunnelID(creds.TunnelID)
	if err != nil {
		return nil, err
	}
	clientID := make([]byte, 16)
	if _, err := rand.Read(clientID); err != nil {
		return nil, fmt.Errorf("generate client id: %w", err)
	}

	ret, err := c.call(ctx, methodRegister, 1, 3, func(s *segment, data, ptrs int) {
		// connIndex = 0
		s.setU8(data, 0, 0)

		auth := s.alloc(0 + 2)
		s.setStructPtr(ptrs, auth, 0, 2)
		s.setText(auth, creds.AccountTag)
		s.setData(auth+1, creds.Secret)

		s.setData(ptrs+1, tunnelID)

		options := s.alloc(1 + 2)
		s.setStructPtr(ptrs+2, options, 1, 2)
		// replaceExisting=false, compressionQuality=0, numPreviousAttempts=0。

		client := s.alloc(0 + 4)
		s.setStructPtr(options+1, client, 0, 4)
		s.setData(client, clientID)
		s.setTextList(client+1, []string{"support_quic_eof"})
		s.setText(client+2, version)
		s.setText(client+3, runtime.GOOS+"_"+runtime.GOARCH)
	})
	if err != nil {
		return nil, err
	}

	content, err := ret.payloadContent()
	if err != nil {
		return nil, fmt.Errorf("decode registration result: %w", err)
	}
	// registerConnection_Results { result :ConnectionResponse }
	respSeg, respPtr, err := ret.msg.ptrAt(content, 0)
	if err != nil {
		return nil, err
	}
	resp, err := ret.msg.structAt(respSeg, respPtr)
	if err != nil {
		return nil, err
	}

	switch ret.msg.u16At(resp, 0) {
	case 0: // error
		errSeg, errPtr, err := ret.msg.ptrAt(resp, 0)
		if err != nil {
			return nil, err
		}
		connErr, err := ret.msg.structAt(errSeg, errPtr)
		if err != nil {
			return nil, err
		}
		cause, _ := ret.msg.textAt(connErr, 0)
		return nil, &registrationError{
			Cause:       cause,
			RetryAfter:  time.Duration(ret.msg.u64At(connErr, 0)),
			ShouldRetry: ret.msg.boolAt(connErr, 64),
		}
	case 1: // connectionDetails
		detailSeg, detailPtr, err := ret.msg.ptrAt(resp, 0)
		if err != nil {
			return nil, err
		}
		details, err := ret.msg.structAt(detailSeg, detailPtr)
		if err != nil {
			return nil, err
		}
		uuid, _ := ret.msg.dataAt(details, 0)
		location, _ := ret.msg.textAt(details, 1)
		return &connectionDetails{
			UUID:            uuid,
			Location:        location,
			RemotelyManaged: ret.msg.boolAt(details, 0),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported registration response arm %d", ret.msg.u16At(resp, 0))
	}
}

// sendLocalConfig 把本地 ingress 规则推给边缘（仅非远程托管隧道需要）。
func (c *controlStream) sendLocalConfig(ctx context.Context, localURL string) error {
	config := buildLocalConfig(localURL)
	_, err := c.call(ctx, methodUpdateLocalConfig, 0, 1, func(s *segment, _ int, ptrs int) {
		s.setData(ptrs, config)
	})
	return err
}

// buildLocalConfig 生成边缘可解析的 ingress JSON。
//
// 只包含一条无 hostname 的规则（匹配所有 Host），service 指向本地 HTTP 服务；
// 末尾的空 hostname 规则同时满足 ingress 的 catch-all 要求。
func buildLocalConfig(localURL string) []byte {
	config := struct {
		Ingress []map[string]any `json:"ingress"`
		Warp    map[string]any   `json:"warp-routing"`
	}{
		Ingress: []map[string]any{
			{"service": localURL, "originRequest": map[string]any{}},
		},
		Warp: map[string]any{},
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return []byte(`{"ingress":[],"warp-routing":{}}`)
	}
	return encoded
}

// drain 持续读取控制流，直到连接关闭或 ctx 取消。
//
// 注册完成后仍必须读：否则边缘发来的控制消息会堆在 QUIC 流里，且连接断开
// 无法及时发现。服务端发起的调用一律以异常回复。
func (c *controlStream) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		msg, err := readMessage(c.br)
		if err != nil {
			return
		}
		root, err := msg.rootStruct()
		if err != nil {
			return
		}
		if msg.u16At(root, 0) == messageCall {
			seg, ptrWord, err := msg.ptrAt(root, 0)
			if err != nil {
				return
			}
			call, err := msg.structAt(seg, ptrWord)
			if err != nil {
				return
			}
			_ = c.send(buildReturnException(msg.u32At(call, 0), "unsupported server call"))
		}
	}
}

// decodeTunnelID 把带/不带连字符的 UUID 转成 16 字节。
func decodeTunnelID(value string) ([]byte, error) {
	hexValue := strings.ReplaceAll(strings.TrimSpace(value), "-", "")
	if len(hexValue) != 32 {
		return nil, fmt.Errorf("invalid tunnel id %q", value)
	}
	return hex.DecodeString(hexValue)
}
