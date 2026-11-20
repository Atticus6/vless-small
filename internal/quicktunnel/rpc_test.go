package quicktunnel

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"
)

// buildTestReturn 构造一个只含 results 的 RPC Return。
func buildTestReturn(questionID uint32, fill func(s *segment, payloadPtr int)) []byte {
	s, rootData, rootPtrs := newMessage(1, 1)
	s.setU16(rootData, 0, messageReturn)
	ret := s.alloc(2 + 1)
	s.setStructPtr(rootPtrs, ret, 2, 1)
	s.setU32(ret, 0, questionID)
	s.setU16(ret, 6, 0) // results
	payload := s.alloc(0 + 2)
	s.setStructPtr(ret+2, payload, 0, 2)
	fill(s, payload)
	return s.encode()
}

func buildTestRegisterResults(questionID uint32, uuid []byte, location string) []byte {
	return buildTestReturn(questionID, func(s *segment, payloadPtr int) {
		results := s.alloc(0 + 1)
		s.setStructPtr(payloadPtr, results, 0, 1)
		resp := s.alloc(1 + 1)
		s.setStructPtr(results, resp, 1, 1)
		s.setU16(resp, 0, 1) // connectionDetails
		details := s.alloc(1 + 2)
		s.setStructPtr(resp+1, details, 1, 2)
		s.setData(details+1, uuid)
		s.setText(details+2, location)
		s.setBit(details, 0, false)
	})
}

func readCallParams(msg *message) (uint32, uint16, structRef, error) {
	root, err := msg.rootStruct()
	if err != nil {
		return 0, 0, structRef{}, err
	}
	if tag := msg.u16At(root, 0); tag != messageCall {
		return 0, 0, structRef{}, fmt.Errorf("expected call, got tag %d", tag)
	}
	seg, ptrWord, err := msg.ptrAt(root, 0)
	if err != nil {
		return 0, 0, structRef{}, err
	}
	call, err := msg.structAt(seg, ptrWord)
	if err != nil {
		return 0, 0, structRef{}, err
	}
	payloadSeg, payloadPtr, err := msg.ptrAt(call, 1)
	if err != nil {
		return 0, 0, structRef{}, err
	}
	payload, err := msg.structAt(payloadSeg, payloadPtr)
	if err != nil {
		return 0, 0, structRef{}, err
	}
	contentSeg, contentPtr, err := msg.ptrAt(payload, 0)
	if err != nil {
		return 0, 0, structRef{}, err
	}
	params, err := msg.structAt(contentSeg, contentPtr)
	if err != nil {
		return 0, 0, structRef{}, err
	}
	return msg.u32At(call, 0), msg.u16At(call, 4), params, nil
}

func TestControlStreamRegistrationFlow(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	serverErr := make(chan error, 1)
	go func() {
		serverErr <- serveTestControl(serverConn)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	control := newControlStream(clientConn)
	if err := control.bootstrap(ctx); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	details, err := control.register(ctx, &Credentials{
		TunnelID:   "14725836-1234-5678-9abc-def012345678",
		AccountTag: "tag",
		Secret:     []byte("secret"),
	}, "v1.2.3")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if details.Location != "HKG" || len(details.UUID) != 16 || details.RemotelyManaged {
		t.Fatalf("details = %+v", details)
	}
	if err := control.sendLocalConfig(ctx, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("sendLocalConfig: %v", err)
	}

	if err := <-serverErr; err != nil {
		t.Fatalf("server: %v", err)
	}
}

func serveTestControl(conn net.Conn) error {
	br := bufio.NewReader(conn)
	write := func(frame []byte) error {
		_, err := conn.Write(frame)
		return err
	}

	// 1. bootstrap
	msg, err := readMessage(br)
	if err != nil {
		return fmt.Errorf("read bootstrap: %w", err)
	}
	root, err := msg.rootStruct()
	if err != nil {
		return err
	}
	if tag := msg.u16At(root, 0); tag != messageBootstrap {
		return fmt.Errorf("bootstrap tag = %d", tag)
	}
	if err := write(buildTestReturn(0, func(*segment, int) {})); err != nil {
		return err
	}

	// 2. registerConnection
	msg, err = readMessage(br)
	if err != nil {
		return fmt.Errorf("read register: %w", err)
	}
	qid, method, params, err := readCallParams(msg)
	if err != nil {
		return err
	}
	if qid != 1 || method != methodRegister {
		return fmt.Errorf("register call qid=%d method=%d", qid, method)
	}
	authSeg, authPtr, err := msg.ptrAt(params, 0)
	if err != nil {
		return err
	}
	auth, err := msg.structAt(authSeg, authPtr)
	if err != nil {
		return err
	}
	account, _ := msg.textAt(auth, 0)
	secret, _ := msg.dataAt(auth, 1)
	tunnelID, _ := msg.dataAt(params, 1)
	optionsSeg, optionsPtr, err := msg.ptrAt(params, 2)
	if err != nil {
		return err
	}
	options, err := msg.structAt(optionsSeg, optionsPtr)
	if err != nil {
		return err
	}
	clientSeg, clientPtr, err := msg.ptrAt(options, 0)
	if err != nil {
		return err
	}
	clientInfo, err := msg.structAt(clientSeg, clientPtr)
	if err != nil {
		return err
	}
	if account != "tag" || string(secret) != "secret" || len(tunnelID) != 16 ||
		msg.u8At(params, 0) != 0 {
		return fmt.Errorf("unexpected register params")
	}
	version, _ := msg.textAt(clientInfo, 2)
	if version != "v1.2.3" {
		return fmt.Errorf("client version = %q", version)
	}
	if err := write(buildTestRegisterResults(1, bytes.Repeat([]byte{0xab}, 16), "HKG")); err != nil {
		return err
	}

	// 3. Finish for register
	msg, err = readMessage(br)
	if err != nil {
		return fmt.Errorf("read register finish: %w", err)
	}
	root, err = msg.rootStruct()
	if err != nil {
		return err
	}
	if tag := msg.u16At(root, 0); tag != messageFinish {
		return fmt.Errorf("register finish tag = %d", tag)
	}

	// 4. updateLocalConfiguration
	msg, err = readMessage(br)
	if err != nil {
		return fmt.Errorf("read local config: %w", err)
	}
	qid, method, params, err = readCallParams(msg)
	if err != nil {
		return err
	}
	if qid != 2 || method != methodUpdateLocalConfig {
		return fmt.Errorf("config call qid=%d method=%d", qid, method)
	}
	config, _ := msg.dataAt(params, 0)
	if !bytes.Contains(config, []byte("http://127.0.0.1:8080")) {
		return fmt.Errorf("local config = %s", config)
	}
	if err := write(buildTestReturn(2, func(*segment, int) {})); err != nil {
		return err
	}

	// 5. Finish for update config
	if _, err := readMessage(br); err != nil {
		return fmt.Errorf("read config finish: %w", err)
	}
	return nil
}

// TestBootstrapContextCancel 验证控制流读取会响应取消，而不是等满固定的
// rpcTimeout：关闭隧道或重连时 Run 必须能及时退出。
func TestBootstrapContextCancel(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	defer server.Close()

	// 边缘不回复，模拟控制流挂住。
	go func() {
		buf := make([]byte, 1024)
		for {
			if _, err := server.Read(buf); err != nil {
				return
			}
		}
	}()

	control := newControlStream(client)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	if err := control.bootstrap(ctx); err == nil {
		t.Fatal("expected canceled bootstrap")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bootstrap ignored cancel for %v", elapsed)
	}
}
