package main

import (
	"runtime"
	"runtime/metrics"
	"sync"
	"time"

	"fogs/proto"
)

// recorder collects the metrics of one task: the stages by the clock, the time
// each kind of work took, the amounts moved and what the agent used. Every method
// is safe from any goroutine, and a nil recorder records nothing.
type recorder struct {
	mu      sync.Mutex
	now     func() time.Time
	started time.Time
	step    string // the stage running now, "" before the first
	stepAt  time.Time
	steps   []proto.MetricTime
	work    []proto.MetricTime // in the order each kind first appeared
	amounts []proto.MetricAmount
	peak    uint64 // the most memory seen
	cpu0    time.Duration
	gc0     uint64
}

func newRecorder() *recorder {
	r := &recorder{now: time.Now}
	r.started = r.now()
	r.cpu0 = processCPU()
	r.gc0, r.peak = runtimeStats()
	return r
}

// Step ends the stage running now and starts the next one.
func (r *recorder) Step(name string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	r.closeStep(now)
	r.step, r.stepAt = name, now
}

func (r *recorder) closeStep(now time.Time) {
	if r.step == "" {
		return
	}
	r.steps = addTime(r.steps, r.step, now.Sub(r.stepAt))
	r.step = ""
}

// Add counts d against a kind of work.
func (r *recorder) Add(kind string, d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.work = addTime(r.work, kind, d)
}

// Time starts timing a kind of work; the returned function stops it.
func (r *recorder) Time(kind string) func() {
	if r == nil {
		return func() {}
	}
	start := r.now()
	return func() { r.Add(kind, r.now().Sub(start)) }
}

// Count adds n to a counted amount.
func (r *recorder) Count(name string, n uint64) { r.amount(name, n, false) }

// Bytes adds n to an amount of bytes.
func (r *recorder) Bytes(name string, n uint64) { r.amount(name, n, true) }

func (r *recorder) amount(name string, n uint64, bytes bool) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.amounts {
		if r.amounts[i].Name == name {
			r.amounts[i].Value += n
			return
		}
	}
	r.amounts = append(r.amounts, proto.MetricAmount{Name: name, Value: n, Bytes: bytes})
}

// Sample notes how much memory the agent holds now. The reports of a task call it,
// so the peak is seen every few seconds.
func (r *recorder) Sample() {
	if r == nil {
		return
	}
	_, held := runtimeStats()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peak = max(r.peak, held)
}

// Result returns the metrics so far, the stage running now counted up to this
// moment. It changes nothing: a failure report and a later result may both ask.
func (r *recorder) Result() *proto.TaskMetrics {
	if r == nil {
		return nil
	}
	r.Sample()
	gc, _ := runtimeStats()
	cpu := processCPU()
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	steps := append([]proto.MetricTime(nil), r.steps...)
	if r.step != "" {
		steps = addTime(steps, r.step, now.Sub(r.stepAt))
	}
	return &proto.TaskMetrics{
		TotalMs:    now.Sub(r.started).Milliseconds(),
		Steps:      steps,
		Work:       append([]proto.MetricTime{}, r.work...),
		Amounts:    append([]proto.MetricAmount{}, r.amounts...),
		CPUMs:      max(cpu-r.cpu0, 0).Milliseconds(),
		Cores:      runtime.NumCPU(),
		PeakMemory: r.peak,
		GCCycles:   uint32(gc - r.gc0),
	}
}

// addTime adds d to the entry called name, or appends one. A stage that comes
// twice, as after a retry, is one entry.
func addTime(list []proto.MetricTime, name string, d time.Duration) []proto.MetricTime {
	ms := d.Milliseconds()
	for i := range list {
		if list[i].Name == name {
			list[i].Ms += ms
			return list
		}
	}
	return append(list, proto.MetricTime{Name: name, Ms: ms})
}

// runtimeStats returns the garbage collections so far and the memory the Go
// runtime holds from the system. It reads counters and does not stop the program.
func runtimeStats() (gcCycles, held uint64) {
	samples := []metrics.Sample{{Name: "/gc/cycles/total:gc-cycles"}, {Name: "/memory/classes/total:bytes"}}
	metrics.Read(samples)
	if samples[0].Value.Kind() == metrics.KindUint64 {
		gcCycles = samples[0].Value.Uint64()
	}
	if samples[1].Value.Kind() == metrics.KindUint64 {
		held = samples[1].Value.Uint64()
	}
	return gcCycles, held
}
