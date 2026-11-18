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
