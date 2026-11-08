package boot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"fogs/proto"
)

// metricsReport says which report brought the metrics.
type metricsReport struct {
	Kind    string // "failure": an attempt stopped on an error; "result": the task ended
	OK      bool
	Message string
}

// MetricsRecord is one file in the metrics folder: the metrics of one attempt of a
// task with what they belong to, so a tool needs nothing else to compare runs.
type MetricsRecord struct {
	Task    int64              `json:"task"`
	Kind    string             `json:"kind"` // clone, deploy, format
	Image   string             `json:"image,omitempty"`
	Host    string             `json:"host"`
	MAC     string             `json:"mac"`
	Report  string             `json:"report"` // failure or result
	OK      bool               `json:"ok"`
	Message string             `json:"message"`
	SavedAt time.Time          `json:"savedAt"`
	Metrics *proto.TaskMetrics `json:"metrics"`
}

// writeMetricsFile keeps the metrics of an attempt as task-<id>-attempt-<n>.json.
// A later report of the same attempt replaces the file; another attempt gets its
// own. The file appears whole or not at all.
func writeMetricsFile(ctx context.Context, c Config, id int64, m *proto.TaskMetrics, report metricsReport) error {
	if c.MetricsDir == "" {
		return nil
	}
	task, err := c.Agent.TaskInfo(ctx, id)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(MetricsRecord{
		Task: id, Kind: task.Kind, Image: task.Image, Host: task.Host, MAC: task.MAC,
		Report: report.Kind, OK: report.OK, Message: report.Message, SavedAt: time.Now(), Metrics: m,
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(c.MetricsDir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(c.MetricsDir, ".metrics-*.tmp")
	if err != nil {
		return err
	}
	_, err = f.Write(append(data, '\n'))
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(f.Name())
		return err
	}
	os.Chmod(f.Name(), 0o644) // CreateTemp makes it private; a tool reads it
	name := filepath.Join(c.MetricsDir, fmt.Sprintf("task-%d-attempt-%d.json", id, max(m.Attempt, 1)))
	if err := os.Rename(f.Name(), name); err != nil {
		os.Remove(f.Name())
		return err
	}
	return nil
}
