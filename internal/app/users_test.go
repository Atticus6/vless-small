package app

import "testing"

func TestFormatUUIDRoundTrip(t *testing.T) {
	const raw = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	id, err := parseUUID(raw)
	if err != nil {
		t.Fatalf("parseUUID: %v", err)
	}
	if got := formatUUID(id); got != raw {
		t.Fatalf("formatUUID() = %q, want %q", got, raw)
	}
}

func TestFormatUUIDCompactInput(t *testing.T) {
	id, err := parseUUID("AAAAAAAABBBBCCCCDDDDEEEEEEEEEEEE")
	if err != nil {
		t.Fatalf("parseUUID: %v", err)
	}
	if got := formatUUID(id); got != "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee" {
		t.Fatalf("formatUUID() = %q", got)
	}
}
