package main

import (
	"sync/atomic"
	"time"
)

// uiStats counts the work of the screen: it draws a dozen frames a second beside
// the clone or deploy and takes processor time from it. The window updates these;
// the metrics of a task take the difference over the task.
var uiStats struct {
	frames    atomic.Int64
	renderNs  atomic.Int64 // drawing a frame into memory
	presentNs atomic.Int64 // putting it into the window
}

// uiSnapshot returns the screen counters so far.
func uiSnapshot() (frames int64, render, present time.Duration) {
	return uiStats.frames.Load(), time.Duration(uiStats.renderNs.Load()), time.Duration(uiStats.presentNs.Load())
}

// timed runs f and adds its time to a counter.
func timed(counter *atomic.Int64, f func()) {
	start := time.Now()
	f()
	counter.Add(int64(time.Since(start)))
}
