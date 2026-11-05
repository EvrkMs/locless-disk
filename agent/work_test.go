package main

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
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"fogs/agent/ui"
	"fogs/proto"
	"fogs/server/boot"
	"fogs/server/imaging"
	"fogs/server/store"
)

var testMAC = net.HardwareAddr{0x04, 0x7c, 0x16, 0xe7, 0xd2, 0x1e}

// realServer runs the actual client-facing handlers on a real database, so
// these tests cover both ends of the protocol.
func realServer(t *testing.T) (*httptest.Server, *store.Store, store.Host) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "fogs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	host, err := s.AddHost(context.Background(), testMAC, "PC-12")
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	no := func(net.HardwareAddr) bool { return false }
	images := imaging.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv := httptest.NewServer(boot.New(boot.Config{
		HTTPAddr: "unused",
		Known:    no,
		HasTask:  no,
		Started:  func(net.HardwareAddr) {},
		Files:    fstest.MapFS{},
		Agent:    s,
		Clone:    images,
		Deploy:   images,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}))
	t.Cleanup(srv.Close)
	return srv, s, host
}

func testWorker(url string) (*worker, *atomic.Int32) {
	cfg := proto.AgentConfig{Server: url, MAC: testMAC.String(), Host: "PC-12"}
	reboots := new(atomic.Int32)
	return &worker{
		cfg:     cfg,
		client:  newClient(cfg),
		screen:  &screen{},
		localIP: func() string { return "192.168.88.104" },
		sleep:   func(time.Duration) {},
		reboot:  func() { reboots.Add(1) },
		retry:   make(chan struct{}),
	}, reboots
}

func TestCheckTaskRunsToTheEnd(t *testing.T) {
	srv, s, host := realServer(t)
	ctx := context.Background()
	if _, err := s.CreateTask(ctx, host.ID, store.TaskAgent, ""); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	w, reboots := testWorker(srv.URL)
	w.run()

	if reboots.Load() != 1 {
		t.Fatalf("reboots: %d", reboots.Load())
	}
	hosts, _ := s.Hosts(ctx)
	last := hosts[0].LastTask
	if hosts[0].Task != nil || last == nil || last.Status != store.StatusDone || !strings.Contains(last.Message, "192.168.88.104") {
		t.Fatalf("host %+v, last %+v", hosts[0], last)
	}
	st := w.screen.snapshot()
	if st.Phase != ui.PhaseDone || st.Title != "Готово" {
		t.Fatalf("screen: %+v", st)
	}
	if ok, _ := s.HasTask(ctx, testMAC); ok {
		t.Fatal("the PC would boot WinPE again")
	}
}

func TestNoTaskRebootsToDisk(t *testing.T) {
	srv, _, _ := realServer(t)
	w, reboots := testWorker(srv.URL)
	w.run()
	if reboots.Load() != 1 {
		t.Fatalf("reboots: %d", reboots.Load())
	}
	if st := w.screen.snapshot(); st.Title != "Задачи нет" {
		t.Fatalf("title: %q", st.Title)
	}
}

func TestCancelStopsTheCheck(t *testing.T) {
	srv, s, host := realServer(t)
	ctx := context.Background()
	if _, err := s.CreateTask(ctx, host.ID, store.TaskAgent, ""); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, reboots := testWorker(srv.URL)
	sleeps := 0
	w.sleep = func(time.Duration) {
		if sleeps++; sleeps == 3 {
			if err := s.CancelTask(ctx, host.ID); err != nil {
				t.Errorf("CancelTask: %v", err)
			}
		}
	}
	w.run()
	if reboots.Load() != 1 {
		t.Fatalf("reboots: %d", reboots.Load())
	}
	if st := w.screen.snapshot(); st.Title != "Задача отменена" {
		t.Fatalf("title: %q", st.Title)
	}
	if hosts, _ := s.Hosts(ctx); hosts[0].LastTask != nil {
		t.Fatalf("cancelled task got a result: %+v", hosts[0].LastTask)
	}
}

func TestUnknownKindFailsTheTask(t *testing.T) {
	srv, s, host := realServer(t)
	ctx := context.Background()
	if _, err := s.CreateTask(ctx, host.ID, "deploy-from-the-future", ""); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(srv.URL)
	if err := w.attempt(); err == nil {
		t.Fatal("attempt accepted an unknown task")
	}
	hosts, _ := s.Hosts(ctx)
	if last := hosts[0].LastTask; last == nil || last.Status != store.StatusFailed {
		t.Fatalf("last: %+v", last)
	}
}

func TestNoNetworkShowsErrorAndRetryWorks(t *testing.T) {
	srv, _, _ := realServer(t)
	w, reboots := testWorker(srv.URL)
	var up atomic.Bool
	w.localIP = func() string {
		if up.Load() {
			return "192.168.88.104"
		}
		return ""
	}

	done := make(chan struct{})
	go func() {
		w.run()
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for w.screen.snapshot().Phase != ui.PhaseFailed {
		if time.Now().After(deadline) {
			t.Fatal("no error screen")
		}
		time.Sleep(time.Millisecond)
	}
	st := w.screen.snapshot()
	if !strings.Contains(st.Detail, "драйвера") || !strings.Contains(st.Hint, "R") {
		t.Fatalf("screen: %+v", st)
	}
	if st.Stages[stageNetwork].Status != ui.StageFailed {
		t.Fatalf("stages: %+v", st.Stages)
	}
	if !strings.Contains(st.Hint, "Ctrl+Alt+Del") {
		t.Fatalf("hint: %q", st.Hint)
	}
	for !w.failed.Load() { // set right after the screen, the keys rely on it
		if time.Now().After(deadline) {
			t.Fatal("the error state was not set")
		}
		time.Sleep(time.Millisecond)
	}

	up.Store(true)
	w.retry <- struct{}{}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("retry did not finish")
	}
	if reboots.Load() != 1 {
		t.Fatalf("reboots: %d", reboots.Load())
	}
	if w.failed.Load() {
		t.Fatal("reboot keys stayed active after the retry: they could break a running task")
	}
}

func TestServerDownShowsError(t *testing.T) {
	srv, _, _ := realServer(t)
	url := srv.URL
	srv.Close()
	w, _ := testWorker(url)
	err := w.attempt()
	if err == nil || !strings.Contains(err.Error(), "сервер не отвечает") {
		t.Fatalf("got %v", err)
	}
}

// TestLostReportsAreTolerated covers a short network drop in the middle of a
// task: a few failed reports must not stop it, and the result must still arrive.
func TestLostReportsAreTolerated(t *testing.T) {
	var progress, finished atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/agent/task":
			json.NewEncoder(rw).Encode(proto.AgentTask{ID: 7, Kind: proto.TaskKindCheck})
		case strings.HasSuffix(r.URL.Path, "/progress"):
			if n := progress.Add(1); n >= 3 && n <= 6 {
				rw.WriteHeader(http.StatusBadGateway)
				return
			}
			rw.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(r.URL.Path, "/finish"):
			if finished.Add(1) < 3 {
				rw.WriteHeader(http.StatusBadGateway)
				return
			}
			rw.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	w, _ := testWorker(srv.URL)
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if progress.Load() != checkSteps || finished.Load() != 3 {
		t.Fatalf("%d reports, %d finish calls", progress.Load(), finished.Load())
	}
}

func TestSnapshotDoesNotShareState(t *testing.T) {
	s := &screen{}
	s.log("один")
	snap := s.snapshot()
	snap.Log[0] = "изменено"
	if s.snapshot().Log[0] != "один" {
		t.Fatal("snapshot shares the log with the worker")
	}
}
