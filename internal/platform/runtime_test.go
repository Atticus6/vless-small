package platform

import "testing"

func TestDetectRuntimeExplicitOverride(t *testing.T) {
	t.Setenv("NODE_RUNTIME", " Railway ")
	t.Setenv("RUNTIME", "local")

	if got := DetectRuntime(); got != "railway" {
		t.Fatalf("DetectRuntime() = %q, want railway", got)
	}
}

func TestDetectRuntimeFallsBackToRuntimeEnv(t *testing.T) {
	t.Setenv("NODE_RUNTIME", "")
	t.Setenv("RUNTIME", "LOCAL")

	if got := DetectRuntime(); got != "local" {
		t.Fatalf("DetectRuntime() = %q, want local", got)
	}
}
