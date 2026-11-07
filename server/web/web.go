// Package web is the operator panel: a JSON API and the page that uses it.
//
// There is no login yet, so the panel must stay on a loopback address: a task
// can take over a PC, and club customers share the network with the server.
package web

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"fogs/server/store"
)

//go:embed index.html
var indexHTML []byte

const maxNameLen = 64

// taskHistory is how many tasks the panel shows.
const taskHistory = 100

// Store is the part of the database the panel uses.
type Store interface {
	Hosts(context.Context) ([]store.Host, error)
	RecentTasks(context.Context, int) ([]store.HostTask, error)
	TaskMetrics(context.Context, int64) (string, error)
	DiscoveredPCs(context.Context) ([]store.Discovered, error)
	AddHost(context.Context, net.HardwareAddr, string) (store.Host, error)
	RenameHost(context.Context, int64, string) error
	DeleteHost(context.Context, int64) error
	ForgetDiscovered(context.Context, net.HardwareAddr) error
	CreateTask(context.Context, int64, string, string) (store.Task, error)
	CancelTask(context.Context, int64) error
	Setting(context.Context, string) (string, error)
	SetSetting(context.Context, string, string) error
	Images(context.Context) ([]store.Image, error)
	CreateImage(context.Context, string, string) error
	ImagePath(context.Context, string) (string, error)
	DeleteImage(context.Context, string) error
	SetHostImage(context.Context, int64, string) error
	HostImage(context.Context, int64) (string, error)
	AllVersions(context.Context, string) ([]store.Version, error)
	DeleteVersion(context.Context, string, int) error
	ActiveImageTasks(context.Context, string) (int, error)
	ClearImageLinks(context.Context, string) error
}

// Images changes the versions of images, which takes more than the database.
type Images interface {
	Restore(ctx context.Context, image string, number int) error
	Enable(ctx context.Context, image string, number int) error
	StartMerge(ctx context.Context, image string) error
	Merging(image string) (bool, string)
}

// Config is what the panel needs from the server.
type Config struct {
	Store  Store
	Images Images
	Addr   string // host:port the panel listens on
	Root   string // directory served to clients without a login
	Log    *slog.Logger

	Status    func() Status   // how the client services are; nil reports nothing
	Changed   func()          // the settings the client services depend on changed; may be nil
	Addresses func() []string // IPv4 addresses of the machine clients could use
	Bind      string          // the client address fixed at start with -bind, if any
}

// Status says whether PCs of the club are being served.
type Status struct {
	Addr         string    `json:"addr"`                   // where clients reach the server
	Waiting      string    `json:"waiting,omitempty"`      // what is missing before the services start
	Services     []Service `json:"services"`               // the running client services
	MissingFiles []string  `json:"missingFiles,omitempty"` // boot files not in the served folder
}

// Service is one client service.
type Service struct {
	Name    string `json:"name"`
	Running bool   `json:"running"`
	Error   string `json:"error,omitempty"` // why it does not run; it tries again by itself
}

type server struct {
	store  Store
	images Images
	root   string
	log    *slog.Logger
	cfg    Config
}

// New returns the panel handler.
func New(c Config) http.Handler {
	h := &server{store: c.Store, images: c.Images, root: c.Root, log: c.Log, cfg: c}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(indexHTML)
	})
	mux.HandleFunc("GET /api/state", h.state)
	mux.HandleFunc("GET /api/tasks/{id}/metrics", h.taskMetrics)
	mux.HandleFunc("POST /api/hosts", h.addHost)
	mux.HandleFunc("PATCH /api/hosts/{id}", h.renameHost)
	mux.HandleFunc("DELETE /api/hosts/{id}", h.deleteHost)
	mux.HandleFunc("POST /api/hosts/{id}/task", h.createTask)
	mux.HandleFunc("DELETE /api/hosts/{id}/task", h.cancelTask)
	mux.HandleFunc("DELETE /api/discovered/{mac}", h.forgetDiscovered)
	mux.HandleFunc("GET /api/settings", h.settings)
	mux.HandleFunc("PUT /api/settings", h.saveSettings)
	mux.HandleFunc("POST /api/images", h.createImage)
	mux.HandleFunc("DELETE /api/images/{name}", h.deleteImage)
	mux.HandleFunc("DELETE /api/images/{name}/versions/{number}", h.deleteVersion)
	mux.HandleFunc("POST /api/images/{name}/versions/{number}/restore", h.restoreVersion)
	mux.HandleFunc("POST /api/images/{name}/versions/{number}/enable", h.enableVersion)
	mux.HandleFunc("POST /api/images/{name}/merge", h.mergeImage)
	mux.HandleFunc("PUT /api/hosts/{id}/image", h.setHostImage)
	return guard(c.Addr, mux)
}

// guard protects a panel without a login from web pages open in the operator's
// browser. Host stops DNS rebinding; Origin and the JSON content type stop
// cross-site requests, because a form cannot send them.
func guard(addr string, next http.Handler) http.Handler {
	hosts := map[string]bool{addr: true}
	if _, port, err := net.SplitHostPort(addr); err == nil {
		hosts[net.JoinHostPort("localhost", port)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "wrong host", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || !hosts[u.Host] {
				http.Error(w, "wrong origin", http.StatusForbidden)
				return
			}
		}
		if r.Method != http.MethodGet && r.ContentLength != 0 {
			if ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); ct != "application/json" {
				http.Error(w, "expected JSON", http.StatusUnsupportedMediaType)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (h *server) state(w http.ResponseWriter, r *http.Request) {
	hosts, err := h.store.Hosts(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	discovered, err := h.store.DiscoveredPCs(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	images, err := h.store.Images(r.Context())
	if err != nil {
		h.fail(w, err)
		return
	}
	offline, err := h.auditImages(r.Context(), images, hosts)
	if err != nil {
		h.fail(w, err)
		return
	}
	tasks, err := h.store.RecentTasks(r.Context(), taskHistory)
	if err != nil {
		h.fail(w, err)
		return
	}
	status := Status{Services: []Service{}}
	if h.cfg.Status != nil {
		status = h.cfg.Status()
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts, "discovered": discovered, "images": images, "tasks": tasks,
		"storageOffline": offline, "server": status})
}

// changed tells the client services to look at the settings again.
func (h *server) changed() {
	if h.cfg.Changed != nil {
		h.cfg.Changed()
	}
}

func (h *server) addHost(w http.ResponseWriter, r *http.Request) {
	var in struct{ MAC, Name, Image string }
	if !readJSON(w, r, &in) {
		return
	}
	mac, err := net.ParseMAC(strings.TrimSpace(in.MAC))
	if err != nil || len(mac) != 6 {
		writeError(w, http.StatusBadRequest, "Неверный MAC-адрес")
		return
	}
	name, ok := cleanName(w, in.Name)
	if !ok {
		return
	}
	host, err := h.store.AddHost(r.Context(), mac, name)
	if errors.Is(err, store.ErrExists) {
		writeError(w, http.StatusConflict, "Компьютер с таким MAC уже есть")
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	if in.Image != "" {
		if err := h.store.SetHostImage(r.Context(), host.ID, in.Image); err != nil {
			h.store.DeleteHost(r.Context(), host.ID)
			writeError(w, http.StatusConflict, "Такого образа нет")
			return
		}
		host.Image = in.Image
	}
	h.log.Info("host added", "mac", host.MAC, "name", host.Name)
	writeJSON(w, http.StatusCreated, host)
}

func (h *server) renameHost(w http.ResponseWriter, r *http.Request) {
	id, ok := hostID(w, r)
	if !ok {
		return
	}
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	name, ok := cleanName(w, in.Name)
	if !ok {
		return
	}
	h.done(w, h.store.RenameHost(r.Context(), id, name))
}

func (h *server) deleteHost(w http.ResponseWriter, r *http.Request) {
	if id, ok := hostID(w, r); ok {
		h.done(w, h.store.DeleteHost(r.Context(), id))
	}
}

func (h *server) createTask(w http.ResponseWriter, r *http.Request) {
	id, ok := hostID(w, r)
	if !ok {
		return
	}
	in := struct{ Kind string }{Kind: store.TaskAgent}
	if r.ContentLength != 0 && !readJSON(w, r, &in) {
		return
	}
	image, err := h.store.HostImage(r.Context(), id)
	if err != nil {
		h.done(w, err)
		return
	}
	switch in.Kind {
	case store.TaskAgent, store.TaskFormat:
		image = ""
	case store.TaskClone, store.TaskDeploy:
		if image == "" {
			writeError(w, http.StatusConflict, "Сначала выберите образ у компьютера")
			return
		}
		if !h.imageReady(w, r, image, in.Kind == store.TaskDeploy) {
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "Неизвестный тип задачи")
		return
	}
	task, err := h.store.CreateTask(r.Context(), id, in.Kind, image)
	if errors.Is(err, store.ErrTaskExists) {
		writeError(w, http.StatusConflict, "У компьютера уже есть задача")
		return
	}
	if err != nil {
		h.done(w, err)
		return
	}
	h.log.Info("task created", "host", id, "kind", task.Kind)
	writeJSON(w, http.StatusCreated, task)
}

func (h *server) cancelTask(w http.ResponseWriter, r *http.Request) {
	if id, ok := hostID(w, r); ok {
		h.done(w, h.store.CancelTask(r.Context(), id))
	}
}

func (h *server) forgetDiscovered(w http.ResponseWriter, r *http.Request) {
	mac, err := net.ParseMAC(r.PathValue("mac"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "Неверный MAC-адрес")
		return
	}
	h.done(w, h.store.ForgetDiscovered(r.Context(), mac))
}

// done finishes a request that returns no data.
func (h *server) done(w http.ResponseWriter, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "Не найдено")
	default:
		h.fail(w, err)
	}
}

// fail reports an internal error; the details go to the log, not the page.
func (h *server) fail(w http.ResponseWriter, err error) {
	h.log.Error("request failed", "err", err)
	writeError(w, http.StatusInternalServerError, "Ошибка сервера, подробности в логе")
}

func hostID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Неверный номер компьютера")
		return 0, false
	}
	return id, true
}

func cleanName(w http.ResponseWriter, s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > maxNameLen {
		writeError(w, http.StatusBadRequest, "Имя должно быть от 1 до 64 символов")
		return "", false
	}
	return s, true
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "Неверный запрос")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// taskMetrics returns what the agent measured for a task, as it sent it.
func (h *server) taskMetrics(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Неверная задача")
		return
	}
	metrics, err := h.store.TaskMetrics(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) || err == nil && metrics == "" {
		writeError(w, http.StatusNotFound, "Метрик у этой задачи нет")
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, metrics)
}
