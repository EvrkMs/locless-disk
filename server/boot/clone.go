package boot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"

	"fogs/proto"
	"fogs/server/imaging"
	"fogs/server/store"
)

// Limits for one request. A batch of blocks is far below maxPutBody; the agent
// keeps batches small so a lost request costs little.
const (
	maxCheckBody = 1 << 20
	maxPutBody   = 512 << 20
)

// CloneManager receives images.
type CloneManager interface {
	Claim(ctx context.Context, task int64, image string) error
	Start(ctx context.Context, task int64, image, mac string) (proto.CloneInfo, error)
	Check(task int64, hashes []proto.Sum) ([]byte, error)
	Put(task int64, r io.Reader) error
	Finish(ctx context.Context, task int64, m proto.Manifest) (proto.CloneResult, error)
	Abort(task int64)
	// ServerStats returns what the server did for a task: time by kind of work and
	// amounts, for the metrics of the task.
	ServerStats(task int64) ([]proto.MetricTime, []proto.MetricAmount)
	// ResetStats starts them over for a new attempt of the task.
	ResetStats(task int64)
}

// ownTask checks that the sender owns this active task of the given kind. A
// request that fails the check changes nothing, unless the task is over for
// everyone: then what it left on the server is dropped, whoever asked.
func ownTask(w http.ResponseWriter, r *http.Request, c Config, kind string) (*store.Task, net.HardwareAddr, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.Error(w, "bad task id", http.StatusBadRequest)
		return nil, nil, false
	}
	mac, ok := parseMAC(w, r.URL.Query().Get("mac"))
	if !ok {
		return nil, nil, false
	}
	task, err := c.Agent.ActiveTask(r.Context(), mac)
	if err != nil {
		internalError(w, c, err)
		return nil, nil, false
	}
	if task == nil || task.ID != id || task.Kind != kind {
		open, err := c.Agent.TaskOpen(r.Context(), id)
		if err != nil {
			internalError(w, c, err)
			return nil, nil, false
		}
		if !open {
			c.Clone.Abort(id)
		}
		http.Error(w, "no such active task", http.StatusGone)
		return nil, nil, false
	}
	return task, mac, true
}

// addCloneRoutes registers the endpoints a clone goes through.
func addCloneRoutes(mux *http.ServeMux, c Config) {
	cloneTask := func(w http.ResponseWriter, r *http.Request) (*store.Task, net.HardwareAddr, bool) {
		return ownTask(w, r, c, store.TaskClone)
	}
	fail := func(w http.ResponseWriter, err error) {
		switch {
		case errors.Is(err, imaging.ErrNoImageDir), errors.Is(err, imaging.ErrBusy), errors.Is(err, imaging.ErrNoSession):
			http.Error(w, err.Error(), http.StatusConflict)
		default:
			c.Log.Warn("clone request failed", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
	}

	mux.HandleFunc("POST /agent/tasks/{id}/clone/claim", func(w http.ResponseWriter, r *http.Request) {
		task, _, ok := cloneTask(w, r)
		if !ok {
			return
		}
		err := c.Clone.Claim(r.Context(), task.ID, task.Image)
		if errors.Is(err, imaging.ErrBusy) {
			http.Error(w, err.Error(), http.StatusLocked) // not a failure: the agent waits
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /agent/tasks/{id}/clone/start", func(w http.ResponseWriter, r *http.Request) {
		task, mac, ok := cloneTask(w, r)
		if !ok {
			return
		}
		info, err := c.Clone.Start(r.Context(), task.ID, task.Image, mac.String())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, info)
	})

	mux.HandleFunc("POST /agent/tasks/{id}/clone/abort", func(w http.ResponseWriter, r *http.Request) {
		task, _, ok := cloneTask(w, r)
		if !ok {
			return
		}
		c.Clone.Abort(task.ID)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /agent/tasks/{id}/clone/check", func(w http.ResponseWriter, r *http.Request) {
		task, _, ok := cloneTask(w, r)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCheckBody))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		hashes, err := proto.ParseHashes(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		missing, err := c.Clone.Check(task.ID, hashes)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(missing)
	})

	mux.HandleFunc("POST /agent/tasks/{id}/clone/put", func(w http.ResponseWriter, r *http.Request) {
		task, _, ok := cloneTask(w, r)
		if !ok {
			return
		}
		if err := c.Clone.Put(task.ID, http.MaxBytesReader(w, r.Body, maxPutBody)); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("POST /agent/tasks/{id}/clone/finish", func(w http.ResponseWriter, r *http.Request) {
		task, _, ok := cloneTask(w, r)
		if !ok {
			return
		}
		var m proto.Manifest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&m); err != nil {
			http.Error(w, "bad manifest", http.StatusBadRequest)
			return
		}
		res, err := c.Clone.Finish(r.Context(), task.ID, m)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
}
