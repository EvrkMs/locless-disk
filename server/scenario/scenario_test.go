package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fogs/server/store"
)

var (
	ctx = context.Background()
	mac = net.HardwareAddr{2, 0, 0, 0, 0, 1}
)

type env struct {
	s      *store.Store
	r      *Runner
	host   store.Host
	images string
	free   uint64
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	e := &env{s: s, images: filepath.Join(dir, "images"), free: 100 << 30}
	if err := s.SetSetting(ctx, store.SettingImageDir, e.images); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if err := s.AddVersion(ctx, "win11", store.Version{Number: 1, File: "v1.ldisk", Size: 10 << 30}); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if e.host, err = s.AddHost(ctx, mac, "PC-01"); err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	e.r = &Runner{Store: s, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), MetricsDir: filepath.Join(dir, "metrics"),
		FreeSpace: func(string) (uint64, error) { return e.free, nil }}
	return e
}

// finish ends the task of the step running now as the agent would.
func (e *env) finish(t *testing.T, ok bool) {
	t.Helper()
	runs := e.r.Runs()
	step := runs[0].Steps[runningStep(&runs[0])]
	if err := e.s.FinishTask(ctx, mac, step.Task, ok, "сообщение агента"); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	e.r.Advance(ctx)
}

func TestRunGoesThroughTheSteps(t *testing.T) {
	e := newEnv(t)
	run, err := e.r.Start(ctx, e.host.ID, e.host.Name, "win11")
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := e.r.Start(ctx, e.host.ID, e.host.Name, "win11"); !errors.Is(err, ErrRunning) {
		t.Fatalf("second run: %v", err)
	}
	if run.Steps[0].Status != StatusRunning || run.Steps[0].Kind != store.TaskDeploy {
		t.Fatalf("first step %+v", run.Steps[0])
	}
	e.r.Advance(ctx) // the deploy has not ended: nothing moves
	if got := e.r.Runs()[0].Steps[0].Status; got != StatusRunning {
		t.Fatalf("moved early: %s", got)
	}

	e.finish(t, true)
	delta := e.r.Runs()[0].Steps[1]
	if delta.Status != StatusRunning || delta.Kind != store.TaskClone || delta.Image != "win11" {
		t.Fatalf("delta step %+v", delta)
	}
	e.finish(t, true)
	full := e.r.Runs()[0].Steps[2]
	if full.Status != StatusRunning || !strings.HasPrefix(full.Image, "win11-full-") {
		t.Fatalf("full step %+v", full)
	}
	if info, err := os.Stat(filepath.Join(e.images, full.Image)); err != nil || !info.IsDir() {
		t.Fatalf("no folder for the new image: %v", err)
	}
	task, _ := e.s.TaskInfo(ctx, full.Task)
	if task.Image != full.Image || task.Kind != store.TaskClone {
		t.Fatalf("task %+v", task)
	}
	e.finish(t, true)

	done := e.r.Runs()[0]
	if done.Status != StatusDone || done.Finished == nil {
		t.Fatalf("run %+v", done)
	}
	for _, st := range done.Steps {
		if st.Started.Location() != st.Finished.Location() {
			t.Fatalf("step %q starts in %v and ends in %v", st.Name, st.Started.Location(), st.Finished.Location())
		}
	}
	data, err := os.ReadFile(filepath.Join(e.r.MetricsDir, "scenario-"+done.ID+".json"))
	if err != nil {
		t.Fatalf("no scenario file: %v", err)
	}
	var saved Run
	if err := json.Unmarshal(data, &saved); err != nil || len(saved.Steps) != 3 || saved.Steps[2].Task != full.Task {
		t.Fatalf("file %s, err %v", data, err)
	}
	// The PC is free again, so another run may start.
	if _, err := e.r.Start(ctx, e.host.ID, e.host.Name, "win11"); err != nil {
		t.Fatalf("next run: %v", err)
	}
}

func TestFailedStepEndsTheRun(t *testing.T) {
	e := newEnv(t)
	if _, err := e.r.Start(ctx, e.host.ID, e.host.Name, "win11"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e.finish(t, false)
	run := e.r.Runs()[0]
	if run.Status != StatusFailed || run.Steps[0].Status != StatusFailed || run.Steps[1].Status != StatusWaiting ||
		!strings.Contains(run.Error, "сообщение агента") {
		t.Fatalf("run %+v", run)
	}
	if ok, _ := e.s.HasTask(ctx, mac); ok {
		t.Fatal("a failed run queued another task")
	}
}

// TestNoSpaceStopsBeforeTheWholeClone: the server disk is checked before a run and
// again before the whole clone, which needs about the size of the image.
func TestNoSpaceStopsBeforeTheWholeClone(t *testing.T) {
	e := newEnv(t)
	e.free = 10 << 30 // less than 10 GiB + 10% + the margin
	if _, err := e.r.Start(ctx, e.host.ID, e.host.Name, "win11"); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("Start: %v", err)
	}
	if ok, _ := e.s.HasTask(ctx, mac); ok {
		t.Fatal("a refused run queued a task")
	}

	e.free = 100 << 30
	if _, err := e.r.Start(ctx, e.host.ID, e.host.Name, "win11"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	e.finish(t, true)
	e.free = 1 << 30 // the disk filled up meanwhile
	e.finish(t, true)
	run := e.r.Runs()[0]
	if run.Status != StatusFailed || run.Steps[2].Status != StatusFailed || !strings.Contains(run.Error, "not enough free space") {
		t.Fatalf("run %+v", run)
	}
	images, _ := e.s.Images(ctx)
	if len(images) != 1 {
		t.Fatalf("an image was made without room for it: %+v", images)
	}
}

func TestStopCancelsTheTask(t *testing.T) {
	e := newEnv(t)
	if _, err := e.r.Start(ctx, e.host.ID, e.host.Name, "win11"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := e.r.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	run := e.r.Runs()[0]
	if run.Status != StatusStopped || run.Steps[0].Status != StatusStopped {
		t.Fatalf("run %+v", run)
	}
	if ok, _ := e.s.HasTask(ctx, mac); ok {
		t.Fatal("the task was left")
	}
	if err := e.r.Stop(ctx); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second stop: %v", err)
	}
}

func TestImageWithoutVersionsIsRefused(t *testing.T) {
	e := newEnv(t)
	if _, err := e.r.Start(ctx, e.host.ID, e.host.Name, "empty"); !errors.Is(err, ErrNoImage) {
		t.Fatalf("Start: %v", err)
	}
}
