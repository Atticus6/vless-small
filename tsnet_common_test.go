package main

import "testing"

func TestSanitizeTSNetHostname(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"hk-01", "hk-01"},
		{"HK 01", "hk-01"},
		{"my_node", "my-node"},
		{"---", ""},
		{"节点", ""},
		{"a", "a"},
	}

	for _, tc := range cases {
		if got := sanitizeTSNetHostname(tc.input); got != tc.want {
			t.Fatalf("sanitizeTSNetHostname(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	long := ""
	for i := 0; i < 80; i++ {
		long += "a"
	}
	if got := sanitizeTSNetHostname(long); len(got) != 63 {
		t.Fatalf("long hostname length = %d, want 63", len(got))
	}
}
