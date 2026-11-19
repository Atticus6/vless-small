//go:build !vercel

package tunnel

import (
	"testing"

	"github.com/cloudflare/cloudflared/connection"
	"github.com/google/uuid"
)

func TestParseTunnelToken(t *testing.T) {
	token := connection.TunnelToken{
		AccountTag:   "account-tag",
		TunnelID:     uuid.MustParse("f70ff985-a4ef-4643-bbbc-4a0ed4fc8415"),
		TunnelSecret: []byte("0123456789abcdef0123456789abcdef"),
	}
	encoded, err := token.Encode()
	if err != nil {
		t.Fatal(err)
	}

	credentials, err := parseTunnelToken(encoded)
	if err != nil {
		t.Fatalf("parseTunnelToken: %v", err)
	}
	if credentials.AccountTag != token.AccountTag ||
		credentials.TunnelID != token.TunnelID ||
		string(credentials.TunnelSecret) != string(token.TunnelSecret) {
		t.Fatalf("credentials = %+v", credentials)
	}
}

func TestParseTunnelTokenRejectsInvalid(t *testing.T) {
	for _, value := range []string{
		"",
		"not-base64",
		"e30=", // {}
	} {
		if _, err := parseTunnelToken(value); err == nil {
			t.Fatalf("parseTunnelToken(%q) succeeded", value)
		}
	}
}

func TestNormalizeTunnelHostname(t *testing.T) {
	tests := map[string]string{
		"HK-Tunnel.Atticus.EU.org":            "hk-tunnel.atticus.eu.org",
		"https://hk-tunnel.atticus.eu.org/ws": "hk-tunnel.atticus.eu.org",
		" hk-tunnel.atticus.eu.org. ":         "hk-tunnel.atticus.eu.org",
		"":                                    "",
	}
	for input, want := range tests {
		if got := normalizeTunnelHostname(input); got != want {
			t.Fatalf("normalizeTunnelHostname(%q) = %q, want %q", input, got, want)
		}
	}
}
