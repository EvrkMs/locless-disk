package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"fogs/server/web"
)

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

func TestParseBind(t *testing.T) {
	if ip, err := parseBind(""); err != nil || ip.IsValid() {
		t.Fatalf("empty: %v, %v", ip, err)
	}
	if _, err := parseBind("192.168.88.105"); err != nil {
		t.Fatalf("parseBind: %v", err)
	}
	for _, s := range []string{"nope", "0.0.0.0", "::1"} {
		if _, err := parseBind(s); err == nil {
			t.Fatalf("parseBind accepted %q", s)
		}
	}
}

func TestChooseAddr(t *testing.T) {
	a, b := netip.MustParseAddr("192.168.88.254"), netip.MustParseAddr("10.0.0.5")
	for _, c := range []struct {
		name   string
		bind   netip.Addr
		chosen string
		local  []netip.Addr
		want   netip.Addr // invalid: waits
	}{
		{"-bind wins", b, a.String(), []netip.Addr{a}, b},
		{"the panel decides", netip.Addr{}, b.String(), []netip.Addr{a, b}, b},
		{"the only address", netip.Addr{}, "", []netip.Addr{a}, a},
		{"two addresses need a choice", netip.Addr{}, "", []netip.Addr{a, b}, netip.Addr{}},
		{"a chosen address that went away", netip.Addr{}, b.String(), []netip.Addr{a}, netip.Addr{}},
		{"no network", netip.Addr{}, "", nil, netip.Addr{}},
	} {
		got, why := chooseAddr(c.bind, c.chosen, c.local)
		if got != c.want || (why == "") != c.want.IsValid() {
			t.Errorf("%s: %v, %q", c.name, got, why)
		}
	}
}

// TestOldDatabaseStopsTheServer: starting on an empty database would hide every
// PC and image of the club.
func TestOldDatabaseStopsTheServer(t *testing.T) {
	root := t.TempDir()
	db := filepath.Join(root, dataDir, "fogs.db")
	if err := checkOldDatabase(root, db); err != nil {
		t.Fatalf("a fresh folder: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, oldDatabase), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := checkOldDatabase(root, db); err == nil || !strings.Contains(err.Error(), "move") {
		t.Fatalf("got %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(db), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(db, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := checkOldDatabase(root, db); err != nil {
		t.Fatalf("after the move: %v", err)
	}
}

// TestSupervisorKeepsServicesInStep: a failing service comes back and the panel sees
// why it failed; a new address starts the services over, the same one does not.
func TestSupervisorKeepsServicesInStep(t *testing.T) {
	var mu sync.Mutex
	want := setup{waiting: "нет папки"}
	resolve := func(context.Context) setup {
		mu.Lock()
		defer mu.Unlock()
		return want
	}
	var starts, failures atomic.Int32
	services := func(addr netip.Addr) []service {
		starts.Add(1)
		return []service{{name: "test", run: func(ctx context.Context, up func()) error {
			if failures.Add(1) == 1 {
				return errors.New("порт занят")
			}
			up()
			<-ctx.Done()
			return nil
		}}}
	}
	s := newSupervisor(slog.New(slog.NewTextHandler(io.Discard, nil)), resolve, services)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.run(ctx) }()
	defer func() { cancel(); <-done }()

	waitFor(t, func() bool { return s.Status().Waiting == "нет папки" })
	mu.Lock()
	want = setup{addr: netip.MustParseAddr("192.168.88.254")}
	mu.Unlock()
	s.Changed()
	waitFor(t, func() bool {
		st := s.Status()
		return st.Addr == "192.168.88.254" && len(st.Services) == 1 && st.Services[0].Error == "порт занят"
	})
	waitFor(t, func() bool {
		st := s.Status()
		return len(st.Services) == 1 && st.Services[0] == web.Service{Name: "test", Running: true}
	})
	s.Changed() // the same settings again
	time.Sleep(50 * time.Millisecond)
	if n := starts.Load(); n != 1 {
		t.Fatalf("started %d times for one address", n)
	}
	mu.Lock()
	want = setup{addr: netip.MustParseAddr("10.0.0.5")}
	mu.Unlock()
	s.Changed()
	waitFor(t, func() bool { return s.Status().Addr == "10.0.0.5" && starts.Load() == 2 })
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
