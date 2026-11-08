package web

import (
	"errors"
	"net/http"

	"fogs/server/scenario"
	"fogs/server/store"
)

// startScenario starts the measuring scenario on a PC with the image the PC has.
// Its first step deploys, so the disk of that PC is wiped.
func (h *server) startScenario(w http.ResponseWriter, r *http.Request) {
	var in struct{ HostID int64 }
	if !readJSON(w, r, &in) {
		return
	}
	hosts, err := h.store.Hosts(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	var host *store.Host
	for i := range hosts {
		if hosts[i].ID == in.HostID {
			host = &hosts[i]
		}
	}
	switch {
	case host == nil:
		writeError(w, http.StatusNotFound, "Нет такого компьютера")
		return
	case host.Image == "":
		writeError(w, http.StatusConflict, "Сначала выберите образ у компьютера")
		return
	case host.Task != nil:
		writeError(w, http.StatusConflict, "У компьютера уже есть задача")
		return
	}
	if !h.imageReady(w, r, host.Image, true) {
		return
	}
	run, err := h.cfg.Scenarios.Start(r.Context(), host.ID, host.Name, host.Image)
	switch {
	case errors.Is(err, scenario.ErrRunning):
		writeError(w, http.StatusConflict, "Сценарий уже идёт")
	case errors.Is(err, scenario.ErrNoImage):
		writeError(w, http.StatusConflict, "В образе нет версий")
	case errors.Is(err, scenario.ErrNoSpace):
		writeError(w, http.StatusConflict, "На диске образов мало места для полного клона: "+err.Error())
	case errors.Is(err, store.ErrTaskExists):
		writeError(w, http.StatusConflict, "У компьютера уже есть задача")
	case err != nil:
		h.fail(w, err)
	default:
		writeJSON(w, http.StatusCreated, run)
	}
}

// stopScenario stops the run going on and cancels its task.
func (h *server) stopScenario(w http.ResponseWriter, r *http.Request) {
	err := h.cfg.Scenarios.Stop(r.Context())
	if errors.Is(err, scenario.ErrNotFound) {
		writeError(w, http.StatusNotFound, "Сценарий не идёт")
		return
	}
	h.done(w, err)
}
