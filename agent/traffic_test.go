package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRateFollowsTheLastSeconds(t *testing.T) {
	var r rate
	start := time.Unix(1000, 0)
	if got := r.add(start, 0); got != 0 {
		t.Fatalf("first: %v", got)
	}
	// 100 bytes a second for a while, then 1000: the speed must follow the change
	// within the window instead of averaging the whole run.
	total := uint64(0)
	for i := 1; i <= 20; i++ {
		total += 100
		r.add(start.Add(time.Duration(i)*time.Second), total)
	}
	var got float64
	for i := 21; i <= 30; i++ {
		total += 1000
		got = r.add(start.Add(time.Duration(i)*time.Second), total)
	}
	if got != 1000 {
		t.Fatalf("speed %v, want 1000", got)
	}
	if len(r.samples) > int(rateWindow/time.Second)+2 {
		t.Fatalf("kept %d samples", len(r.samples))
	}
}

func TestTrafficCountsBothWays(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		io.WriteString(w, strings.Repeat("x", 3000))
	}))
	defer srv.Close()
	tr := &traffic{}
	c := &http.Client{Transport: countingTransport{base: http.DefaultTransport, t: tr}}
	res, err := c.Post(srv.URL, "text/plain", strings.NewReader(strings.Repeat("y", 500)))
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if tr.up.Load() != 500 || tr.down.Load() != 3000 {
		t.Fatalf("up %d, down %d", tr.up.Load(), tr.down.Load())
	}
}
