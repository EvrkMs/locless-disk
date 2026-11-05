package main

import "testing"

func TestParseIP(t *testing.T) {
	if _, err := parseIP("192.168.88.105"); err != nil {
		t.Fatalf("parseIP: %v", err)
	}
	for _, s := range []string{"", "nope", "0.0.0.0", "::1"} {
		if _, err := parseIP(s); err == nil {
			t.Fatalf("parseIP accepted %q", s)
		}
	}
}
