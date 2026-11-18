package main

import "testing"

func TestCertControllerDisabledNoop(t *testing.T) {
	ctl := newCertController(nil)
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

func TestCertRuntimeAllowed(t *testing.T) {
	for _, runtime := range []string{
		"vercel",
		"railway",
		"render",
		"fly",
		"koyeb",
		"cloudflare",
		"zeabur",
		"northflank",
		"NORTHFLANK",
	} {
		if certRuntimeAllowed(runtime) {
			t.Fatalf("certRuntimeAllowed(%q) = true, want false", runtime)
		}
	}

	for _, runtime := range []string{"", "local", "linux", "unknown"} {
		if !certRuntimeAllowed(runtime) {
			t.Fatalf("certRuntimeAllowed(%q) = false, want true", runtime)
		}
	}
}
