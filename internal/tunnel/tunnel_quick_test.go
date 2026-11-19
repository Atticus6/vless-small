//go:build !vercel

package tunnel

import "testing"

func TestSelectTunnelMode(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		hostname string
		want     tunnelMode
	}{
		{
			name:     "token 和 hostname 都有时使用固定隧道",
			token:    "token",
			hostname: "hk-tunnel.atticus.eu.org",
			want:     tunnelModeFixed,
		},
		{
			name:     "未分配时回退临时隧道",
			token:    "",
			hostname: "",
			want:     tunnelModeQuick,
		},
		{
			name:     "只有 token 时回退临时隧道",
			token:    "token",
			hostname: "",
			want:     tunnelModeQuick,
		},
		{
			name:     "只有 hostname 时回退临时隧道",
			token:    "",
			hostname: "hk-tunnel.atticus.eu.org",
			want:     tunnelModeQuick,
		},
		{
			name:     "空白字段不视为已分配",
			token:    "   ",
			hostname: "  ",
			want:     tunnelModeQuick,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := selectTunnelMode(tt.token, tt.hostname); got != tt.want {
				t.Fatalf(
					"selectTunnelMode(%q, %q) = %v, want %v",
					tt.token, tt.hostname, got, tt.want,
				)
			}
		})
	}
}

func TestParseQuickTunnelResponse(t *testing.T) {
	const id = "550e8400-e29b-41d4-a716-446655440000"
	body := []byte(`{
		"success": true,
		"result": {
			"id": "` + id + `",
			"name": "random-words",
			"hostname": "https://Random-Words.trycloudflare.com/",
			"account_tag": "quick",
			"secret": "c2VjcmV0"
		}
	}`)

	quick, err := parseQuickTunnelResponse(body)
	if err != nil {
		t.Fatalf("parseQuickTunnelResponse: %v", err)
	}
	if quick.URL != "https://random-words.trycloudflare.com" {
		t.Fatalf("URL = %q", quick.URL)
	}
	if quick.Credentials.AccountTag != "quick" {
		t.Fatalf("AccountTag = %q", quick.Credentials.AccountTag)
	}
	if string(quick.Credentials.TunnelSecret) != "secret" {
		t.Fatalf("TunnelSecret = %q", quick.Credentials.TunnelSecret)
	}
	if quick.Credentials.TunnelID.String() != id {
		t.Fatalf("TunnelID = %s", quick.Credentials.TunnelID)
	}
}

func TestParseQuickTunnelResponseRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "success=false",
			body: `{"success":false,"errors":[{"code":1001,"message":"denied"}]}`,
		},
		{
			name: "非法 id",
			body: `{"success":true,"result":{"id":"bad","hostname":"a.trycloudflare.com","account_tag":"quick","secret":"c2VjcmV0"}}`,
		},
		{
			name: "缺少 hostname",
			body: `{"success":true,"result":{"id":"550e8400-e29b-41d4-a716-446655440000","hostname":"","account_tag":"quick","secret":"c2VjcmV0"}}`,
		},
		{
			name: "缺少 secret",
			body: `{"success":true,"result":{"id":"550e8400-e29b-41d4-a716-446655440000","hostname":"a.trycloudflare.com","account_tag":"quick"}}`,
		},
		{
			name: "非法 JSON",
			body: `{`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseQuickTunnelResponse([]byte(tt.body)); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}
