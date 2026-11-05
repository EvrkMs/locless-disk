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

func TestParseMACs(t *testing.T) {
	set, err := parseMACs(" AA-BB-CC-00-00-01, aa:bb:cc:00:00:02 ,")
	if err != nil {
		t.Fatalf("parseMACs: %v", err)
	}
	if len(set) != 2 || !set["aa:bb:cc:00:00:01"] || !set["aa:bb:cc:00:00:02"] {
		t.Fatalf("set: %v", set)
	}

	empty, err := parseMACs("")
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty list: %v %v", empty, err)
	}

	if _, err := parseMACs("nope"); err == nil {
		t.Fatal("parseMACs accepted garbage")
	}
}
