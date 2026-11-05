package main

import "testing"

// TestPanelStaysLocal guards the panel: it has no login and can take over PCs.
func TestPanelStaysLocal(t *testing.T) {
	if err := checkAdminAddr("127.0.0.1:8081"); err != nil {
		t.Fatalf("checkAdminAddr: %v", err)
	}
	for _, s := range []string{"", "8081", "0.0.0.0:8081", "192.168.88.105:8081", ":8081"} {
		if err := checkAdminAddr(s); err == nil {
			t.Fatalf("checkAdminAddr accepted %q", s)
		}
	}
}

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
