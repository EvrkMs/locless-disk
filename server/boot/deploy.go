package boot

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"fogs/proto"
	"fogs/server/imaging"
	"fogs/server/store"
)

// DeployManager sends images.
type DeployManager interface {
	StartDeploy(ctx context.Context, task int64, image string) (proto.DeployInfo, error)
	DeployList(task int64, w io.Writer) error
	DeployBlocks(task int64, numbers []uint32, w io.Writer) error
}

// addDeployRoutes registers the endpoints a deploy goes through.
func addDeployRoutes(mux *http.ServeMux, c Config) {
	deployTask := func(w http.ResponseWriter, r *http.Request) (*store.Task, bool) {
		task, _, ok := ownTask(w, r, c, store.TaskDeploy)
		return task, ok
	}
	fail := func(w http.ResponseWriter, err error) {
		c.Log.Warn("deploy request failed", "err", err)
		code := http.StatusBadRequest
		if errors.Is(err, imaging.ErrNoImageDir) || errors.Is(err, imaging.ErrNoImage) || errors.Is(err, imaging.ErrNoSession) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
	}

	mux.HandleFunc("POST /agent/tasks/{id}/deploy/start", func(w http.ResponseWriter, r *http.Request) {
		task, ok := deployTask(w, r)
		if !ok {
			return
		}
		info, err := c.Deploy.StartDeploy(r.Context(), task.ID, task.Image)
		if errors.Is(err, imaging.ErrBusy) {
			http.Error(w, err.Error(), http.StatusLocked) // not a failure: the agent waits
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(info)
	})

	mux.HandleFunc("POST /agent/tasks/{id}/deploy/abort", func(w http.ResponseWriter, r *http.Request) {
		if task, ok := deployTask(w, r); ok {
			c.Clone.Abort(task.ID)
			w.WriteHeader(http.StatusNoContent)
		}
	})

	mux.HandleFunc("POST /agent/tasks/{id}/deploy/list", func(w http.ResponseWriter, r *http.Request) {
		task, ok := deployTask(w, r)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if err := c.Deploy.DeployList(task.ID, w); err != nil {
			fail(w, err)
		}
	})

	mux.HandleFunc("POST /agent/tasks/{id}/deploy/blocks", func(w http.ResponseWriter, r *http.Request) {
		task, ok := deployTask(w, r)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxCheckBody))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		numbers, err := proto.ParseNumbers(body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		if err := c.Deploy.DeployBlocks(task.ID, numbers, w); err != nil {
			// The status is gone once data has been sent. The agent sees a body
			// that ends too early and stops.
			c.Log.Warn("deploy blocks failed", "err", err)
		}
	})
}
