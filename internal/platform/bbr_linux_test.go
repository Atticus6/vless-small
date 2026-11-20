//go:build linux

package platform

import "testing"

func TestHasBBR(t *testing.T) {
	tests := []struct {
		available string
		want      bool
	}{
		{available: "reno cubic bbr", want: true},
		{available: "bbr", want: true},
		{available: "reno cubic", want: false},
		{available: "", want: false},
		{available: "bbrmod cubic", want: false},
	}

	for _, tt := range tests {
		if got := hasBBR(tt.available); got != tt.want {
			t.Fatalf("hasBBR(%q) = %v, want %v", tt.available, got, tt.want)
		}
	}
}
