package app

import "testing"

func TestFormatUUIDRoundTrip(t *testing.T) {
	const raw = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	id, err := parseUUID(raw)
	if err != nil {
		t.Fatalf("parseUUID: %v", err)
	}
	if got := formatUUID(id); got != raw {
		t.Fatalf("formatUUID() = %q, want %q", got, raw)
	}
}

func TestFormatUUIDCompactInput(t *testing.T) {
	id, err := parseUUID("AAAAAAAABBBBCCCCDDDDEEEEEEEEEEEE")
	if err != nil {
		t.Fatalf("parseUUID: %v", err)
	}
	if got := formatUUID(id); got != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("formatUUID() = %q", got)
	}
}

// TestAuthenticateUUID 验证 hy2 认证复用 VLESS 用户表：只有表里的 UUID 通过，
// 大小写/无连字符写法同样有效，非法输入与空串一律拒绝。
func TestAuthenticateUUID(t *testing.T) {
	const raw = "11111111-1111-4111-8111-111111111111"
	users, err := ParseUsers(raw)
	if err != nil {
		t.Fatalf("ParseUsers: %v", err)
	}
	srv := NewServer(users, "")

	cases := []struct {
		name string
		auth string
		want bool
	}{
		{"标准写法", raw, true},
		{"大写", "11111111-1111-4111-8111-111111111111", true},
		{"无连字符", "11111111111141118111111111111111", true},
		{"其它 UUID", "22222222-2222-4222-8222-222222222222", false},
		{"非 UUID", "password", false},
		{"空串", "", false},
	}
	for _, tc := range cases {
		if got := srv.AuthenticateUUID(tc.auth); got != tc.want {
			t.Errorf("%s: AuthenticateUUID(%q) = %v, want %v", tc.name, tc.auth, got, tc.want)
		}
	}
}

// TestAuthenticateUUIDNoUsers 验证没有用户时不会误放行（包括全零 UUID）。
func TestAuthenticateUUIDNoUsers(t *testing.T) {
	srv := NewServer(nil, "")

	if srv.AuthenticateUUID("11111111-1111-4111-8111-111111111111") {
		t.Fatal("empty user table should reject every uuid")
	}
	if srv.AuthenticateUUID("00000000-0000-0000-0000-000000000000") {
		t.Fatal("nil user table should reject the zero uuid")
	}
}
