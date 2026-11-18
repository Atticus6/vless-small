package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"v0.1.0", "v0.1.0", 0},
		{"v0.1.9", "v0.1.10", -1},
		{"v0.2.0", "v0.1.99", 1},
		{"1.0", "v1.0.0", 0},
		{"dev", "v0.1.0", -1},
		{"", "v0.1.0", -1},
		{"v2", "v1.9.9", 1},
	}

	for _, tc := range cases {
		if got := compareVersions(tc.a, tc.b); got != tc.want {
			t.Fatalf("compareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestAssetArch(t *testing.T) {
	cases := []struct {
		goarch, goarm, want string
	}{
		{"amd64", "", "amd64"},
		{"arm64", "", "arm64"},
		{"arm", "6", "armv6"},
		{"arm", "7", "armv7"},
		{"arm", "", "armv7"},
		{"arm", "v7", "armv7"},
	}

	for _, tc := range cases {
		if got := assetArch(tc.goarch, tc.goarm); got != tc.want {
			t.Fatalf("assetArch(%q, %q) = %q, want %q", tc.goarch, tc.goarm, got, tc.want)
		}
	}
}

func TestInstallUpdate(t *testing.T) {
	payload := []byte("#!/bin/sh\necho updated\n")
	sum := sha256.Sum256(payload)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write(payload)
	}))
	defer ts.Close()

	dir := t.TempDir()
	exePath := filepath.Join(dir, "vless-small")
	if err := os.WriteFile(exePath, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	err := installUpdate(
		nodeConfig{token: "secret"},
		nodeUpdate{
			Version: "v0.2.0",
			URL:     ts.URL,
			SHA256:  hex.EncodeToString(sum[:]),
		},
		exePath,
	)
	if err != nil {
		t.Fatalf("installUpdate: %v", err)
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("installed content = %q", got)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(exePath)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0755 {
			t.Fatalf("mode = %o, want 755", info.Mode().Perm())
		}
	}
}

func TestInstallUpdateChecksumMismatch(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer ts.Close()

	dir := t.TempDir()
	exePath := filepath.Join(dir, "vless-small")
	if err := os.WriteFile(exePath, []byte("old"), 0755); err != nil {
		t.Fatal(err)
	}

	err := installUpdate(
		nodeConfig{token: "secret"},
		nodeUpdate{Version: "v0.2.0", URL: ts.URL, SHA256: "deadbeef"},
		exePath,
	)
	if err == nil {
		t.Fatal("expected checksum error")
	}

	got, err := os.ReadFile(exePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old" {
		t.Fatalf("original binary changed: %q", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

func TestInstallUpdateRejectsIncomplete(t *testing.T) {
	dir := t.TempDir()
	exePath := filepath.Join(dir, "vless-small")

	cases := []nodeUpdate{
		{},
		{Version: "v0.2.0"},
		{Version: "v0.2.0", URL: "http://example.com"},
		{URL: "http://example.com", SHA256: "abc"},
	}
	for _, update := range cases {
		if err := installUpdate(nodeConfig{}, update, exePath); err == nil {
			t.Fatalf("expected error for %#v", update)
		}
	}
}
