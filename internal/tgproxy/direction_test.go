package tgproxy

import (
	"encoding/hex"
	"testing"
)

// TestCipherDirections 锁定 makeStream.start() 的四个 cipher 方向。
// 向量由 JS 参考实现生成：cipher(raw,skey,false) 解密客户端数据、
// cipher(raw,skey,true) 加密回客户端数据。方向搞反会导致数据面对不上。
func TestCipherDirections(t *testing.T) {
	head, _ := hex.DecodeString(dirGolden.Head)
	skey, _ := hex.DecodeString(dirGolden.SKey)

	// cIn = cipher(head, skey, false)：解密客户端发来的外层数据。
	cIn, err := cipherFor(head, skey, false)
	if err != nil {
		t.Fatal(err)
	}
	clientEnc, _ := hex.DecodeString(dirGolden.ClientToServer)
	got := cIn.crypt(append([]byte(nil), clientEnc...))
	plain, _ := hex.DecodeString(dirGolden.Plain)
	if hex.EncodeToString(got) != hex.EncodeToString(plain) {
		t.Fatalf("cIn direction wrong: got %x want %x", got, plain)
	}

	// cOut = cipher(head, skey, true)：加密回客户端。
	cOut, err := cipherFor(head, skey, true)
	if err != nil {
		t.Fatal(err)
	}
	got2 := cOut.crypt(append([]byte(nil), plain...))
	want2, _ := hex.DecodeString(dirGolden.ServerToClient)
	if hex.EncodeToString(got2) != hex.EncodeToString(want2) {
		t.Fatalf("cOut direction wrong: got %x want %x", got2, want2)
	}
}
