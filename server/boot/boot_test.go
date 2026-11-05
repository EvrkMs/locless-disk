package boot

import (
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, method, url string) (int, string) {
	t.Helper()
	known := func(mac net.HardwareAddr) bool { return mac.String() == "aa:bb:cc:00:00:01" }
	h := New(known, slog.New(slog.NewTextHandler(io.Discard, nil)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, url, nil))
	return w.Code, w.Body.String()
}

func TestKnownHost(t *testing.T) {
	code, body := get(t, http.MethodGet, "/boot.ipxe?mac=AA-BB-CC-00-00-01")
	if code != http.StatusOK {
		t.Fatalf("status %d", code)
	}
	if !strings.HasPrefix(body, "#!ipxe\n") || !strings.Contains(body, "aa:bb:cc:00:00:01") || !strings.HasSuffix(body, "exit\n") {
		t.Fatalf("script: %q", body)
	}
}

func TestUnknownHostBootsFromDisk(t *testing.T) {
	code, body := get(t, http.MethodGet, "/boot.ipxe?mac=aa:bb:cc:00:00:02")
	if code != http.StatusOK || body != exitScript {
		t.Fatalf("status %d, script %q", code, body)
	}
}

func TestBadRequests(t *testing.T) {
	tests := []struct {
		name, method, url string
		want              int
	}{
		{"no mac", http.MethodGet, "/boot.ipxe", http.StatusBadRequest},
		{"bad mac", http.MethodGet, "/boot.ipxe?mac=nope", http.StatusBadRequest},
		{"long mac", http.MethodGet, "/boot.ipxe?mac=00:00:00:00:fe:80:00:00", http.StatusBadRequest},
		{"wrong method", http.MethodPost, "/boot.ipxe?mac=aa:bb:cc:00:00:01", http.StatusMethodNotAllowed},
		{"other path", http.MethodGet, "/other", http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if code, _ := get(t, tc.method, tc.url); code != tc.want {
				t.Fatalf("status %d, want %d", code, tc.want)
			}
		})
	}
}
