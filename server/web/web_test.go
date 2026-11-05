package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"fogs/server/store"
)

const addr = "127.0.0.1:8081"

func panel(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "fogs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return New(s, addr, slog.New(slog.NewTextHandler(io.Discard, nil))), s
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = addr
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type state struct {
	Hosts      []store.Host
	Discovered []store.Discovered
}

func getState(t *testing.T, h http.Handler) state {
	t.Helper()
	w := do(h, http.MethodGet, "/api/state", "")
	if w.Code != http.StatusOK {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	var s state
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return s
}

func TestPageIsServed(t *testing.T) {
	h, _ := panel(t)
	w := do(h, http.MethodGet, "/", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<title>FOGs</title>") {
		t.Fatalf("status %d", w.Code)
	}
}

func TestEmptyStateIsEmptyLists(t *testing.T) {
	h, _ := panel(t)
	body := do(h, http.MethodGet, "/api/state", "").Body.String()
	if !strings.Contains(body, `"hosts":[]`) || !strings.Contains(body, `"discovered":[]`) {
		t.Fatalf("the page expects lists, not null: %s", body)
	}
}

func TestHostAndTaskFlow(t *testing.T) {
	h, s := panel(t)

	mac := net.HardwareAddr{0x04, 0x7c, 0x16, 0xe7, 0xd2, 0x1e}
	if err := s.Seen(context.Background(), mac); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if st := getState(t, h); len(st.Discovered) != 1 {
		t.Fatalf("discovered: %+v", st.Discovered)
	}

	w := do(h, http.MethodPost, "/api/hosts", `{"mac":"04-7C-16-E7-D2-1E","name":" PC-01 "}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	st := getState(t, h)
	if len(st.Hosts) != 1 || st.Hosts[0].Name != "PC-01" || st.Hosts[0].MAC != mac.String() || len(st.Discovered) != 0 {
		t.Fatalf("state: %+v", st)
	}
	id := st.Hosts[0].ID
	path := "/api/hosts/" + jsonInt(id)

	if w := do(h, http.MethodPost, "/api/hosts", `{"mac":"04:7c:16:e7:d2:1e","name":"x"}`); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", w.Code)
	}
	if w := do(h, http.MethodPatch, path, `{"name":"PC-02"}`); w.Code != http.StatusNoContent {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, path+"/task", ""); w.Code != http.StatusCreated {
		t.Fatalf("task: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, path+"/task", ""); w.Code != http.StatusConflict {
		t.Fatalf("second task: %d", w.Code)
	}
	if st := getState(t, h); st.Hosts[0].Task == nil || st.Hosts[0].Name != "PC-02" {
		t.Fatalf("host: %+v", st.Hosts[0])
	}
	if w := do(h, http.MethodDelete, path+"/task", ""); w.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodDelete, path, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", w.Code)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestForgetDiscovered(t *testing.T) {
	h, s := panel(t)
	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 1}
	if err := s.Seen(context.Background(), mac); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if w := do(h, http.MethodDelete, "/api/discovered/aa:bb:cc:00:00:01", ""); w.Code != http.StatusNoContent {
		t.Fatalf("forget: %d %s", w.Code, w.Body)
	}
	if st := getState(t, h); len(st.Discovered) != 0 {
		t.Fatalf("discovered: %+v", st.Discovered)
	}
}

func TestRejectsBadInput(t *testing.T) {
	h, _ := panel(t)
	tests := []struct {
		name, method, path, body string
		want                     int
	}{
		{"bad mac", http.MethodPost, "/api/hosts", `{"mac":"nope","name":"x"}`, http.StatusBadRequest},
		{"long mac", http.MethodPost, "/api/hosts", `{"mac":"00:00:00:00:fe:80:00:00","name":"x"}`, http.StatusBadRequest},
		{"empty name", http.MethodPost, "/api/hosts", `{"mac":"aa:bb:cc:00:00:01","name":"  "}`, http.StatusBadRequest},
		{"long name", http.MethodPost, "/api/hosts", `{"mac":"aa:bb:cc:00:00:01","name":"` + strings.Repeat("я", 65) + `"}`, http.StatusBadRequest},
		{"broken json", http.MethodPost, "/api/hosts", `{`, http.StatusBadRequest},
		{"bad id", http.MethodDelete, "/api/hosts/abc", "", http.StatusBadRequest},
		{"task for a missing host", http.MethodPost, "/api/hosts/42/task", "", http.StatusNotFound},
		{"nothing to cancel", http.MethodDelete, "/api/hosts/42/task", "", http.StatusNotFound},
		{"bad discovered mac", http.MethodDelete, "/api/discovered/nope", "", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(h, tc.method, tc.path, tc.body); w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestGuard covers attacks through the operator's own browser: the panel has
// no login, so a hostile page must not be able to drive it.
func TestGuard(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"same origin works", func(r *http.Request) { r.Header.Set("Origin", "http://"+addr) }, http.StatusCreated},
		{"localhost works", func(r *http.Request) { r.Host = "localhost:8081" }, http.StatusCreated},
		{"foreign host (DNS rebinding)", func(r *http.Request) { r.Host = "evil.example" }, http.StatusForbidden},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden},
		{"form post", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := panel(t)
			body := `{"mac":"aa:bb:cc:00:00:01","name":"x"}`
			r := httptest.NewRequest(http.MethodPost, "/api/hosts", strings.NewReader(body))
			r.Host = addr
			r.Header.Set("Content-Type", "application/json")
			tc.mutate(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}
