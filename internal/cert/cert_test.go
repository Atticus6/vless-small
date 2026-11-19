package cert

import "testing"

func TestCertControllerDisabledNoop(t *testing.T) {
	ctl := New(nil)
	defer ctl.Close()

	ctl.Sync("")
	status := ctl.Status()
	if status.Enabled {
		t.Fatal("empty domain should not enable cert")
	}
	if status.Domain != "" {
		t.Fatalf("domain = %q, want empty", status.Domain)
	}
}

// TestShouldSyncCert 覆盖 Sync 的短路判断：
// 已启用的相同域名、已关闭且没有残留域名时跳过，其余情况都要重新应用。
func TestShouldSyncCert(t *testing.T) {
	tests := []struct {
		name          string
		currentDomain string
		enabled       bool
		nextDomain    string
		want          bool
	}{
		{name: "相同域名且已启用", currentDomain: "node.example.com", enabled: true, nextDomain: "node.example.com", want: true},
		{name: "相同域名但启动失败需要重试", currentDomain: "node.example.com", enabled: false, nextDomain: "node.example.com", want: false},
		{name: "关闭且没有残留域名", currentDomain: "", enabled: false, nextDomain: "", want: true},
		{name: "首次下发域名", currentDomain: "", enabled: false, nextDomain: "node.example.com", want: false},
		{name: "关闭已启用的证书", currentDomain: "node.example.com", enabled: true, nextDomain: "", want: false},
		{name: "切换域名", currentDomain: "a.example.com", enabled: true, nextDomain: "b.example.com", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldSyncCert(tt.currentDomain, tt.enabled, tt.nextDomain); got != tt.want {
				t.Fatalf("shouldSyncCert(%q, %v, %q) = %v, want %v",
					tt.currentDomain, tt.enabled, tt.nextDomain, got, tt.want)
			}
		})
	}
}

func TestCertRuntimeAllowed(t *testing.T) {
	for _, runtime := range []string{
		"northflank",
		"NORTHFLANK",
		"railway",
		"render",
		"fly",
		"koyeb",
		"cloudflare",
		"zeabur",
		"",
		"linux",
		"unknown",
	} {
		if certRuntimeAllowed(runtime) {
			t.Fatalf("certRuntimeAllowed(%q) = true, want false", runtime)
		}
	}

	for _, runtime := range []string{"local", "LOCAL", " local "} {
		if !certRuntimeAllowed(runtime) {
			t.Fatalf("certRuntimeAllowed(%q) = false, want true", runtime)
		}
	}
}
