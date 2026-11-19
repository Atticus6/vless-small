package platform

import "testing"

func TestDetectRuntimeExplicitOverride(t *testing.T) {
	t.Setenv("NODE_RUNTIME", " Vercel ")
	t.Setenv("RUNTIME", "local")

	if got := DetectRuntime(); got != "vercel" {
		t.Fatalf("DetectRuntime() = %q, want vercel", got)
	}
}

func TestDetectRuntimeFallsBackToRuntimeEnv(t *testing.T) {
	t.Setenv("NODE_RUNTIME", "")
	t.Setenv("RUNTIME", "LOCAL")

	if got := DetectRuntime(); got != "local" {
		t.Fatalf("DetectRuntime() = %q, want local", got)
	}
}
