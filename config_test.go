package main

import (
	"os"
	"testing"
)

// TestParseFlagsTwice 回归测试：cloudflared 依赖的 CoreDNS 会向全局
// flag.CommandLine 注册 version，解析必须使用独立 FlagSet 且可重复调用。
func TestParseFlagsTwice(t *testing.T) {
	originalArgs := os.Args
	defer func() { os.Args = originalArgs }()

	t.Setenv("PORT", "")
	t.Setenv("UUID", "")
	t.Setenv("WS_PATH", "")
	t.Setenv("CONFIG", "")

	os.Args = []string{"vless-small"}
	first := parseFlags()
	second := parseFlags()

	if first.listen != ":8080" || second.listen != ":8080" {
		t.Fatalf("listen = %q / %q, want :8080", first.listen, second.listen)
	}

	os.Args = []string{"vless-small", "-version"}
	if !parseFlags().showVersion {
		t.Fatal("showVersion = false, want true")
	}
}
