package main

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"

	"fogs/agent/ui"
)

// traffic counts the bytes the agent sends to the server and receives from it:
// request and response bodies, which is nearly all there is. The screen shows the
// network apart from the disk, since compression and blocks the server already
// has make the two differ a lot.
type traffic struct {
	up, down atomic.Uint64
}

// countingTransport counts the bodies of every request through it.
type countingTransport struct {
	base http.RoundTripper
	t    *traffic
}

func (c countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.ContentLength > 0 {
		c.t.up.Add(uint64(req.ContentLength))
	}
	res, err := c.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	res.Body = countingBody{res.Body, &c.t.down}
	return res, nil
}

type countingBody struct {
	io.ReadCloser
	n *atomic.Uint64
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n.Add(uint64(n))
	return n, err
}

// rateWindow is how far back a speed looks: long enough to smooth out batches,
// short enough to follow a change.
const rateWindow = 5 * time.Second

// rate tells how fast a growing total grows over the last rateWindow.
type rate struct {
	samples []rateSample
}

type rateSample struct {
	at    time.Time
	total uint64
}

// add notes the total at now and returns the speed per second. The first call
// has nothing to compare with and returns 0.
func (r *rate) add(now time.Time, total uint64) float64 {
	r.samples = append(r.samples, rateSample{now, total})
	// Keep one sample at or before the start of the window, so the speed always
	// spans the whole window once there is enough history.
	for len(r.samples) > 2 && now.Sub(r.samples[1].at) >= rateWindow {
		r.samples = r.samples[1:]
	}
	first := r.samples[0]
	seconds := now.Sub(first.at).Seconds()
	if seconds <= 0 || total < first.total {
		return 0
	}
	return float64(total-first.total) / seconds
}

// formatRate shows a speed for people.
func formatRate(perSecond float64) string {
	return formatBytes(uint64(perSecond)) + "/с"
}

// speeds follows the disk and both directions of the network during a clone or a
// deploy. The disk moves whole blocks; the network only what is compressed and
// not on the other side yet, so the two are shown apart.
type speeds struct {
	disk, down, up rate
}

// update notes the totals at now and returns what the screen shows and a line for
// the panel.
func (s *speeds) update(now time.Time, diskLabel string, disk uint64, t *traffic) ([]ui.Rate, string) {
	d := formatRate(s.disk.add(now, disk))
	down := formatRate(s.down.add(now, t.down.Load()))
	up := formatRate(s.up.add(now, t.up.Load()))
	rates := []ui.Rate{{Label: diskLabel, Value: d}, {Label: "Сеть ↓ загрузка", Value: down}, {Label: "Сеть ↑ отдача", Value: up}}
	return rates, "диск " + d + ", сеть ↓ " + down + " ↑ " + up
}
