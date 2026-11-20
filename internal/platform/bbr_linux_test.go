//go:build linux && !nobbr

package platform

import "testing"

func TestHasBBR(t *testing.T) {
	tests := []struct {
		available string
		want      bool
	}{
		{available: "reno cubic bbr", want: true},
		{available: "bbr", want: true},
		{available: "reno cubic", want: false},
		{available: "", want: false},
		{available: "bbrmod cubic", want: false},
	}

	for _, tt := range tests {
		if got := hasBBR(tt.available); got != tt.want {
			t.Fatalf("hasBBR(%q) = %v, want %v", tt.available, got, tt.want)
		}
	}
}

// TestIsContainerizedReturnsBool 只验证函数在真实环境里可调用且结果稳定：
// 容器标记取决于运行环境，不能断言具体值，但重复调用必须一致。
func TestIsContainerizedReturnsBool(t *testing.T) {
	first := isContainerized()
	for i := 0; i < 3; i++ {
		if got := isContainerized(); got != first {
			t.Fatalf("isContainerized() changed between calls: %v then %v", first, got)
		}
	}
}
