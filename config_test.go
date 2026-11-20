package main

import (
	"os"
	"testing"
)

// TestParseFlagsTwice 回归测试：解析必须使用独立 FlagSet 且可重复调用，
// 不能被依赖库注册到全局 flag.CommandLine 的参数影响。
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

// TestHy2PortMatchesSubscription 锁定 hy2 端口约定。
//
// 端口在主控（apps/web 的 HY2_PORT）和节点（hy2Port）各写一份，订阅直接
// 用主控的值、节点按自己的值监听——两边不一致时客户端会连到没人监听的
// 端口，而单测和构建都不会报错。这里把节点侧的值钉死，改端口时必须同时
// 改主控并更新这个断言。
func TestHy2PortMatchesSubscription(t *testing.T) {
	if hy2Port != 9500 {
		t.Fatalf("hy2Port = %d, want 9500 (同步修改 apps/web 的 HY2_PORT)", hy2Port)
	}
}
