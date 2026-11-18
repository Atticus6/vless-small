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

func TestCertRuntimeAllowed(t *testing.T) {
	for _, runtime := range []string{
		"northflank",
		"NORTHFLANK",
		"vercel",
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
