// Package scenario runs a measuring run on one test PC: deploy an image, clone it
// back as a delta, then clone it whole into a new image. Each step is an ordinary
// task, and the next one is queued when the one before has ended, so the PC goes
// through all three by booting from the network after each. It is for development:
// the server offers it only with -dev.
package scenario

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"fogs/proto"
	"fogs/server/store"
)

// Store is the part of the database a run uses.
type Store interface {
	CreateTask(ctx context.Context, hostID int64, kind, image string) (store.Task, error)
	CancelTask(ctx context.Context, hostID int64) error
	TaskInfo(ctx context.Context, id int64) (store.HostTask, error)
	CreateImage(ctx context.Context, name, path string) error
	ImagePath(ctx context.Context, name string) (string, error)
	LatestVersion(ctx context.Context, image string) (*store.Version, error)
}

// Statuses of a run and of its steps.
const (
	StatusWaiting = "waiting" // a step not started yet
	StatusRunning = "running"
	StatusDone    = "done"
	StatusFailed  = "failed"
	StatusStopped = "stopped" // the operator stopped the run
)

// Step is one task of a run.
type Step struct {
	Name     string     `json:"name"`
	Kind     string     `json:"kind"`
	Image    string     `json:"image"`
	Task     int64      `json:"task,omitempty"`
	Status   string     `json:"status"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
}

// Run is one pass of the scenario.
type Run struct {
	ID       string     `json:"id"`
	HostID   int64      `json:"hostId"`
	Host     string     `json:"host"`
	Image    string     `json:"image"`
	Steps    []Step     `json:"steps"`
	Status   string     `json:"status"`
	Error    string     `json:"error,omitempty"`
	Started  time.Time  `json:"started"`
	Finished *time.Time `json:"finished,omitempty"`
}

// Errors a caller can tell apart.
var (
	ErrRunning  = errors.New("scenario: a run is going on")
	ErrNoImage  = errors.New("scenario: the image has no versions")
	ErrNoSpace  = errors.New("scenario: not enough free space for the images")
	ErrNotFound = errors.New("scenario: no run is going on")
)

// spaceMargin is what a run leaves free on the disk of the images: the database,
// the log and the clones of other PCs may live there too.
const spaceMargin = 5 << 30

// keepRuns bounds the runs the panel lists.
const keepRuns = 10

// Runner runs one scenario at a time. The runs live in memory: a restart of the
// server forgets them, while their tasks and metrics stay.
type Runner struct {
	Store      Store
	Log        *slog.Logger
	MetricsDir string                            // gets scenario-<id>.json at the end of a run; "" for none
	FreeSpace  func(path string) (uint64, error) // nil asks the disk
	Poll       time.Duration                     // how often the steps are looked at; zero means 5 s

	mu   sync.Mutex
	runs []*Run // newest last
}

// Start begins a run on a PC with an image that has versions. The PC must have no
// task: the first step is queued at once.
func (r *Runner) Start(ctx context.Context, hostID int64, host, image string) (Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.current() != nil {
		return Run{}, ErrRunning
	}
	latest, err := r.Store.LatestVersion(ctx, image)
	if err != nil {
		return Run{}, err
	}
	if latest == nil {
		return Run{}, ErrNoImage
	}
	// The whole clone at the end needs about as much as the newest version. It is
	// checked here as well: better to refuse now than after the deploy.
	if err := r.checkSpace(ctx, image, uint64(latest.Size)); err != nil {
		return Run{}, err
	}
	now := time.Now()
	run := &Run{
		ID: now.Format("20060102-150405"), HostID: hostID, Host: host, Image: image, Status: StatusRunning, Started: now,
		Steps: []Step{
			{Name: "Развёртывание", Kind: store.TaskDeploy, Image: image, Status: StatusWaiting},
			{Name: "Дельта-клон", Kind: store.TaskClone, Image: image, Status: StatusWaiting},
			{Name: "Полный клон", Kind: store.TaskClone, Status: StatusWaiting}, // its image is made when it starts
		},
	}
	if err := r.startStep(ctx, run, 0); err != nil {
		return Run{}, err
	}
	r.runs = append(r.runs, run)
	if len(r.runs) > keepRuns {
		r.runs = r.runs[len(r.runs)-keepRuns:]
	}
	r.Log.Info("scenario started", "run", run.ID, "host", host, "image", image)
	return copyRun(run), nil
}

// Stop ends the run going on and cancels its task.
func (r *Runner) Stop(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.current()
	if run == nil {
		return ErrNotFound
	}
	if err := r.Store.CancelTask(ctx, run.HostID); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	for i := range run.Steps {
		if run.Steps[i].Status == StatusRunning {
			run.Steps[i].Status = StatusStopped
		}
	}
	r.end(run, StatusStopped, "")
	return nil
}

// Runs lists the recent runs, newest first.
func (r *Runner) Runs() []Run {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Run, 0, len(r.runs))
	for i := len(r.runs) - 1; i >= 0; i-- {
		out = append(out, copyRun(r.runs[i]))
	}
	return out
}

func copyRun(run *Run) Run {
	c := *run
	c.Steps = append([]Step(nil), run.Steps...)
	return c
}

// Run looks at the run going on until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	poll := r.Poll
	if poll == 0 {
		poll = 5 * time.Second
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			r.Advance(ctx)
		}
	}
}

// Advance moves the run on when its step has ended: to the next step, or to the end.
func (r *Runner) Advance(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	run := r.current()
	if run == nil {
		return
	}
	i := runningStep(run)
	if i < 0 {
		return
	}
	step := &run.Steps[i]
	task, err := r.Store.TaskInfo(ctx, step.Task)
	if err != nil {
		r.Log.Warn("scenario step unknown", "run", run.ID, "task", step.Task, "err", err)
		return
	}
	if task.FinishedAt == nil {
		return
	}
	step.Finished = task.FinishedAt
	if task.Status != store.StatusDone {
		step.Status = StatusFailed
		r.end(run, StatusFailed, fmt.Sprintf("%s: %s", step.Name, task.Message))
		return
	}
	step.Status = StatusDone
	if i+1 == len(run.Steps) {
		r.end(run, StatusDone, "")
		return
	}
	if err := r.startStep(ctx, run, i+1); err != nil {
		run.Steps[i+1].Status = StatusFailed
		r.end(run, StatusFailed, fmt.Sprintf("%s: %v", run.Steps[i+1].Name, err))
	}
}

// startStep queues the task of a step. The whole clone gets a new, empty image
// first, so it sends every block; the space for it is checked again just before.
func (r *Runner) startStep(ctx context.Context, run *Run, i int) error {
	step := &run.Steps[i]
	if step.Image == "" {
		latest, err := r.Store.LatestVersion(ctx, run.Image)
		if err != nil {
			return err
		}
		size := uint64(0)
		if latest != nil {
			size = uint64(latest.Size)
		}
		if err := r.checkSpace(ctx, run.Image, size); err != nil {
			return err
		}
		name := fmt.Sprintf("%.40s-full-%s", run.Image, time.Now().Format("0102-150405"))
		path, err := r.Store.ImagePath(ctx, name)
		if err != nil {
			return err
		}
		if err := r.Store.CreateImage(ctx, name, path); err != nil {
			return err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return err
		}
		step.Image = name
	}
	task, err := r.Store.CreateTask(ctx, run.HostID, step.Kind, step.Image)
	if err != nil {
		return err
	}
	now := time.Now()
	step.Task, step.Status, step.Started = task.ID, StatusRunning, &now
	r.Log.Info("scenario step started", "run", run.ID, "step", step.Name, "task", task.ID, "image", step.Image)
	return nil
}

// checkSpace refuses a whole clone that the disk of the images cannot hold with
// the margin left over. need is the size of the newest version, which a clone of
// the same disk comes close to.
func (r *Runner) checkSpace(ctx context.Context, image string, need uint64) error {
	dir, err := r.Store.ImagePath(ctx, image)
	if err != nil {
		return err
	}
	free := r.FreeSpace
	if free == nil {
		free = freeSpace
	}
	place := filepath.Dir(dir)
	have, err := free(place)
	if err != nil {
		return fmt.Errorf("scenario: free space of %s: %w", place, err)
	}
	if want := need + need/10 + spaceMargin; have < want {
		return fmt.Errorf("%w: free %d MiB, need %d MiB", ErrNoSpace, have>>20, want>>20)
	}
	return nil
}

func (r *Runner) current() *Run {
	if len(r.runs) == 0 {
		return nil
	}
	if run := r.runs[len(r.runs)-1]; run.Status == StatusRunning {
		return run
	}
	return nil
}

func runningStep(run *Run) int {
	for i, s := range run.Steps {
		if s.Status == StatusRunning {
			return i
		}
	}
	return -1
}

// end closes a run and leaves scenario-<id>.json next to the metrics of its tasks:
// which task was which step, for a tool that compares runs.
func (r *Runner) end(run *Run, status, reason string) {
	now := time.Now()
	run.Status, run.Error, run.Finished = status, reason, &now
	r.Log.Info("scenario ended", "run", run.ID, "status", status, "reason", reason)
	if r.MetricsDir == "" {
		return
	}
	data, err := json.MarshalIndent(struct {
		*Run
		MetricsVersion int `json:"metricsVersion"`
	}{run, proto.MetricsVersion}, "", "  ")
	if err == nil {
		err = os.MkdirAll(r.MetricsDir, 0o755)
	}
	if err == nil {
		err = os.WriteFile(filepath.Join(r.MetricsDir, "scenario-"+run.ID+".json"), append(data, '\n'), 0o644)
	}
	if err != nil {
		r.Log.Warn("scenario file not written", "run", run.ID, "err", err)
	}
}
