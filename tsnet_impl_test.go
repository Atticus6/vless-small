//go:build !vercel

package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTSNetStateFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	payload := `{"Version":"v1","PrivateNodeKey":"secret"}`
	encoded := base64.StdEncoding.EncodeToString([]byte(payload))

	if err := writeTSNetFile(dir, tsnetStateFile, encoded); err != nil {
		t.Fatalf("writeTSNetFile: %v", err)
	}

	got, err := readTSNetFile(dir, tsnetStateFile)
	if err != nil {
		t.Fatalf("readTSNetFile: %v", err)
	}
	if got != payload {
		t.Fatalf("state = %q, want %q", got, payload)
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(dir, tsnetStateFile))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("mode = %o, want 600", info.Mode().Perm())
		}
	}
}

func TestTSNetStateFileEmptyIsNoop(t *testing.T) {
	dir := t.TempDir()

	if err := writeTSNetFile(dir, tsnetLogConfFile, ""); err != nil {
		t.Fatalf("writeTSNetFile: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, tsnetLogConfFile)); !os.IsNotExist(err) {
		t.Fatalf("empty log conf should not create a file, err = %v", err)
	}
}
