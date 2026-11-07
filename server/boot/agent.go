package boot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	"fogs/proto"
	"fogs/server/store"
)

// maxReportText bounds the text an agent may store. Anyone on the network can
// reach these endpoints, so what they accept must stay small and harmless.
const maxReportText = 500

// maxTableBody fits a table of 128 partitions with room to spare.
const maxTableBody = 64 << 10

// AgentStore is the part of the database the agent endpoints use.
type AgentStore interface {
	HostName(context.Context, net.HardwareAddr) (string, error)
	ActiveTask(context.Context, net.HardwareAddr) (*store.Task, error)
	TaskOpen(ctx context.Context, id int64) (bool, error)
	SavedTable(context.Context, net.HardwareAddr) ([]byte, error)
	SetSavedTable(context.Context, net.HardwareAddr, []byte) error
	ReportProgress(ctx context.Context, mac net.HardwareAddr, id int64, percent int, stage, message string) error
	FinishTask(ctx context.Context, mac net.HardwareAddr, id int64, ok bool, message string) error
	SaveMetrics(ctx context.Context, mac net.HardwareAddr, id int64, metrics string) error
}

// addAgentRoutes registers what the agent talks to.
func addAgentRoutes(mux *http.ServeMux, c Config) {
	mux.HandleFunc("GET /agent/{mac}/"+proto.AgentConfigName, func(w http.ResponseWriter, r *http.Request) {
		mac, ok := parseMAC(w, r.PathValue("mac"))
		if !ok {
			return
		}
		name, err := c.Agent.HostName(r.Context(), mac)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			internalError(w, c, err)
			return
		}
		writeJSON(w, http.StatusOK, proto.AgentConfig{Server: "http://" + c.HTTPAddr, MAC: mac.String(), Host: name})
	})

	mux.HandleFunc("GET /agent/task", func(w http.ResponseWriter, r *http.Request) {
		mac, ok := parseMAC(w, r.URL.Query().Get("mac"))
		if !ok {
			return
		}
		task, err := c.Agent.ActiveTask(r.Context(), mac)
		if err != nil {
			internalError(w, c, err)
			return
		}
		if task == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if took, ok := c.boots.agentArrived(mac.String(), task.ID); ok {
			c.Log.Info("WinPE started", "mac", mac.String(), "task", task.ID, "took", took.Round(100*time.Millisecond).String())
		}
		table, err := c.Agent.SavedTable(r.Context(), mac)
		if err != nil {
			internalError(w, c, err)
			return
		}
		writeJSON(w, http.StatusOK, proto.AgentTask{ID: task.ID, Kind: task.Kind, Image: task.Image, Table: table})
	})

	// The partition table of a PC, saved before its volume is shrunk and removed,
	// with an empty body, once the volume is back. It belongs to the PC and not to
	// a task: a cancelled task does not give the volume back.
	mux.HandleFunc("POST /agent/table", func(w http.ResponseWriter, r *http.Request) {
		mac, ok := parseMAC(w, r.URL.Query().Get("mac"))
		if !ok {
			return
		}
		table, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxTableBody))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		err = c.Agent.SetSavedTable(r.Context(), mac, table)
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			internalError(w, c, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	addCloneRoutes(mux, c)
	addDeployRoutes(mux, c)

	mux.HandleFunc("POST /agent/tasks/{id}/progress", func(w http.ResponseWriter, r *http.Request) {
		var in proto.AgentProgress
		id, mac, ok := readReport(w, r, &in, func() string { return in.MAC })
		if !ok {
			return
		}
		if in.Percent < 0 || in.Percent > 100 {
			http.Error(w, "bad percent", http.StatusBadRequest)
			return
		}
		if in.Stage == proto.StageFailed {
			c.Log.Warn("agent reported an error", "mac", mac.String(), "task", id, "message", clip(in.Message), "details", clipDetails(in.Details))
		}
		// Before the report and whatever its answer: a task cancelled meanwhile keeps
		// how far it got. SaveMetrics takes only the PC of the task.
		saveMetrics(r.Context(), c, mac, id, in.Metrics)
		err := c.Agent.ReportProgress(r.Context(), mac, id, in.Percent, clip(in.Stage), clip(in.Message))
		finishReport(w, c, err)
	})

	mux.HandleFunc("POST /agent/tasks/{id}/finish", func(w http.ResponseWriter, r *http.Request) {
		var in proto.AgentResult
		id, mac, ok := readReport(w, r, &in, func() string { return in.MAC })
		if !ok {
			return
		}
		saveMetrics(r.Context(), c, mac, id, in.Metrics)
		err := c.Agent.FinishTask(r.Context(), mac, id, in.OK, clip(in.Message))
		if err == nil {
			c.boots.forget(id)
			if c.Clone != nil {
				c.Clone.Abort(id) // nothing to drop after a successful clone
			}
			c.Log.Info("task finished", "mac", mac.String(), "task", id, "ok", in.OK, "message", clip(in.Message))
		}
		finishReport(w, c, err)
	})
}

// readReport decodes a report and returns the task id and the sender's MAC.
func readReport(w http.ResponseWriter, r *http.Request, in any, macOf func() string) (int64, net.HardwareAddr, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return 0, nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxReportBody)
	if err := json.NewDecoder(r.Body).Decode(in); err != nil {
		http.Error(w, "bad report", http.StatusBadRequest)
		return 0, nil, false
	}
	mac, ok := parseMAC(w, macOf())
	return id, mac, ok
}

// finishReport answers a report. 410 tells the agent to stop: the task is gone.
func finishReport(w http.ResponseWriter, c Config, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		http.Error(w, "no such active task", http.StatusGone)
	default:
		internalError(w, c, err)
	}
}

func parseMAC(w http.ResponseWriter, s string) (net.HardwareAddr, bool) {
	mac, err := net.ParseMAC(s)
	if err != nil || len(mac) != 6 {
		http.Error(w, "bad mac", http.StatusBadRequest)
		return nil, false
	}
	return mac, true
}

func internalError(w http.ResponseWriter, c Config, err error) {
	c.Log.Error("agent request failed", "err", err)
	http.Error(w, "server error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// clip cuts text to maxReportText characters without breaking UTF-8.
func clip(s string) string {
	if utf8.RuneCountInString(s) <= maxReportText {
		return s
	}
	return string([]rune(s)[:maxReportText])
}

// maxReportBody bounds a report: a message, a stack and the metrics of a task.
const maxReportBody = 32 << 10

// maxMetricEntries bounds each list of the metrics; the agent sends about ten.
const maxMetricEntries = 64

// saveMetrics keeps what the agent measured for a task and writes a short line of
// it to the log. Metrics only explain a task, so a failure here fails no report.
func saveMetrics(ctx context.Context, c Config, mac net.HardwareAddr, id int64, m *proto.TaskMetrics) {
	if m == nil {
		return
	}
	if len(m.Steps) > maxMetricEntries || len(m.Work) > maxMetricEntries || len(m.Amounts) > maxMetricEntries {
		c.Log.Warn("metrics refused: too many entries", "mac", mac.String(), "task", id)
		return
	}
	for _, list := range [][]proto.MetricTime{m.Steps, m.Work} {
		for i := range list {
			list[i].Name = clip(list[i].Name)
		}
	}
	for i := range m.Amounts {
		m.Amounts[i].Name = clip(m.Amounts[i].Name)
	}
	m.Server = serverMetrics(c, id) // only the server knows its side
	data, err := json.Marshal(m)
	if err == nil {
		err = c.Agent.SaveMetrics(ctx, mac, id, string(data))
	}
	if err != nil {
		c.Log.Warn("metrics not saved", "mac", mac.String(), "task", id, "err", err)
		return
	}
	amounts := logAmounts(m.Amounts)
	c.Log.Info("task metrics", "mac", mac.String(), "task", id,
		"total", millis(m.TotalMs), "cpu", millis(m.CPUMs), "cores", m.Cores,
		"peakMemoryMiB", m.PeakMemory>>20, "gc", m.GCCycles,
		slog.Group("steps", times(m.Steps)...), slog.Group("work", times(m.Work)...), slog.Group("amounts", amounts...))
	if s := m.Server; s != nil {
		c.Log.Info("task metrics of the server", "mac", mac.String(), "task", id,
			"boot", millis(s.BootMs), slog.Group("work", times(s.Work)...), slog.Group("amounts", logAmounts(s.Amounts)...))
	}
}

// serverMetrics collects the side of the server for a task, or nil if it has none.
func serverMetrics(c Config, id int64) *proto.ServerMetrics {
	s := &proto.ServerMetrics{BootMs: c.boots.bootTime(id).Milliseconds()}
	if c.Clone != nil {
		s.Work, s.Amounts = c.Clone.ServerStats(id)
	}
	if s.BootMs == 0 && len(s.Work) == 0 && len(s.Amounts) == 0 {
		return nil
	}
	return s
}

// logAmounts turns amounts into log attributes, bytes in MiB.
func logAmounts(list []proto.MetricAmount) []any {
	out := make([]any, 0, 2*len(list))
	for _, a := range list {
		if a.Bytes {
			out = append(out, a.Name+", MiB", a.Value>>20)
		} else {
			out = append(out, a.Name, a.Value)
		}
	}
	return out
}

// times turns named spans into log attributes, each as a duration.
func times(list []proto.MetricTime) []any {
	out := make([]any, 0, 2*len(list))
	for _, t := range list {
		out = append(out, t.Name, millis(t.Ms))
	}
	return out
}

func millis(ms int64) string { return (time.Duration(ms) * time.Millisecond).String() }

// maxReportDetails bounds the stack of a crash in the log; the agent sends 4000 bytes.
const maxReportDetails = 4096

// clipDetails cuts a stack to maxReportDetails bytes without breaking UTF-8.
func clipDetails(s string) string {
	if len(s) <= maxReportDetails {
		return s
	}
	s = s[:maxReportDetails]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
