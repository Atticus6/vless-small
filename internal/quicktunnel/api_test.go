package quicktunnel

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestSuccess(t *testing.T) {
	secret := []byte("super-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("content-type = %q", got)
		}
		_, _ = w.Write([]byte(`{
			"success": true,
			"result": {
				"id": "14725836-1234-5678-9abc-def012345678",
				"name": "demo",
				"hostname": "demo.trycloudflare.com",
				"account_tag": "tag",
				"secret": "` + base64.StdEncoding.EncodeToString(secret) + `"
			},
			"errors": []
		}`))
	}))
	defer server.Close()

	creds, err := Request(context.Background(), server.URL, "test-agent")
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if creds.TunnelID != "14725836-1234-5678-9abc-def012345678" {
		t.Fatalf("tunnel id = %q", creds.TunnelID)
	}
	if creds.AccountTag != "tag" || string(creds.Secret) != string(secret) {
		t.Fatalf("credentials = %+v", creds)
	}
	if creds.URL() != "https://demo.trycloudflare.com" {
		t.Fatalf("url = %q", creds.URL())
	}
}

func TestRequestAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1000,"message":"nope"}]}`))
	}))
	defer server.Close()

	_, err := Request(context.Background(), server.URL, "")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestRejectsIncompleteCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"success":true,"result":{"id":"","hostname":"x","account_tag":"","secret":""}}`))
	}))
	defer server.Close()

	if _, err := Request(context.Background(), server.URL, ""); err == nil {
		t.Fatal("expected incomplete credentials error")
	}
}

func TestCredentialsURL(t *testing.T) {
	if got := (Credentials{Hostname: "demo.trycloudflare.com"}).URL(); got != "https://demo.trycloudflare.com" {
		t.Fatalf("url = %q", got)
	}
	if got := (Credentials{Hostname: "https://demo.trycloudflare.com"}).URL(); got != "https://demo.trycloudflare.com" {
		t.Fatalf("url = %q", got)
	}
}
