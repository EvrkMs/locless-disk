package web

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"fogs/server/imaging"
	"fogs/server/store"
)

const addr = "127.0.0.1:8081"

func panel(t *testing.T) (http.Handler, *store.Store) {
	t.Helper()
	h, s, _ := panelWithRoot(t)
	return h, s
}

// panelWithRoot also returns the directory served to clients.
func panelWithRoot(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "fogs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	root := filepath.Join(dir, "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := New(Config{Store: s, Images: imaging.New(s, log), Addr: addr, Root: root, Log: log})
	return h, s, root
}

func saveDir(h http.Handler, dir string) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]string{"imageDir": dir})
	return do(h, http.MethodPut, "/api/settings", string(body))
}

func TestImageDirIsCreatedAndKept(t *testing.T) {
	h, _, root := panelWithRoot(t)
	dir := filepath.Join(filepath.Dir(root), "images", "club")

	if w := saveDir(h, "  "+dir+"  "); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("the write check left files behind: %v", entries)
	}

	var got struct{ ImageDir, Root string }
	w := do(h, http.MethodGet, "/api/settings", "")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ImageDir != dir || got.Root != root {
		t.Fatalf("settings: %+v", got)
	}
}

// TestImageDirStaysOutOfRoot guards the images: everything under -root is
// served to the whole network without a login.
func TestImageDirStaysOutOfRoot(t *testing.T) {
	h, s, root := panelWithRoot(t)
	for name, dir := range map[string]string{
		"root itself": root,
		"below root":  filepath.Join(root, "images"),
		"sneaky path": filepath.Join(root, "a", "..", "images"),
	} {
		t.Run(name, func(t *testing.T) {
			if w := saveDir(h, dir); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
		})
	}
	if v, _ := s.Setting(context.Background(), store.SettingImageDir); v != "" {
		t.Fatalf("a refused path was stored: %q", v)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatalf("a refused path left something in the served directory: %v", entries)
	}
}

func TestImageDirNextToRootIsFine(t *testing.T) {
	h, _, root := panelWithRoot(t)
	if w := saveDir(h, root+"-images"); w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestImageDirRejectsBadPaths(t *testing.T) {
	h, _, root := panelWithRoot(t)
	file := filepath.Join(filepath.Dir(root), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	for name, dir := range map[string]string{
		"empty":    "   ",
		"relative": "images",
		"a file":   file,
	} {
		t.Run(name, func(t *testing.T) {
			if w := saveDir(h, dir); w.Code != http.StatusBadRequest {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
		})
	}
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = addr
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

type state struct {
	Hosts          []store.Host
	Discovered     []store.Discovered
	Images         []store.Image
	StorageOffline bool
}

func getState(t *testing.T, h http.Handler) state {
	t.Helper()
	w := do(h, http.MethodGet, "/api/state", "")
	if w.Code != http.StatusOK {
		t.Fatalf("state: %d %s", w.Code, w.Body)
	}
	var s state
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return s
}

func TestPageIsServed(t *testing.T) {
	h, _ := panel(t)
	w := do(h, http.MethodGet, "/", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<title>FOGs</title>") {
		t.Fatalf("status %d", w.Code)
	}
}

func TestEmptyStateIsEmptyLists(t *testing.T) {
	h, _ := panel(t)
	body := do(h, http.MethodGet, "/api/state", "").Body.String()
	if !strings.Contains(body, `"hosts":[]`) || !strings.Contains(body, `"discovered":[]`) || !strings.Contains(body, `"images":[]`) {
		t.Fatalf("the page expects lists, not null: %s", body)
	}
}

func TestHostAndTaskFlow(t *testing.T) {
	h, s := panel(t)

	mac := net.HardwareAddr{0x04, 0x7c, 0x16, 0xe7, 0xd2, 0x1e}
	if err := s.Seen(context.Background(), mac); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if st := getState(t, h); len(st.Discovered) != 1 {
		t.Fatalf("discovered: %+v", st.Discovered)
	}

	w := do(h, http.MethodPost, "/api/hosts", `{"mac":"04-7C-16-E7-D2-1E","name":" PC-01 "}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	st := getState(t, h)
	if len(st.Hosts) != 1 || st.Hosts[0].Name != "PC-01" || st.Hosts[0].MAC != mac.String() || len(st.Discovered) != 0 {
		t.Fatalf("state: %+v", st)
	}
	id := st.Hosts[0].ID
	path := "/api/hosts/" + jsonInt(id)

	if w := do(h, http.MethodPost, "/api/hosts", `{"mac":"04:7c:16:e7:d2:1e","name":"x"}`); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", w.Code)
	}
	if w := do(h, http.MethodPatch, path, `{"name":"PC-02"}`); w.Code != http.StatusNoContent {
		t.Fatalf("rename: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, path+"/task", ""); w.Code != http.StatusCreated {
		t.Fatalf("task: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, path+"/task", ""); w.Code != http.StatusConflict {
		t.Fatalf("second task: %d", w.Code)
	}
	if st := getState(t, h); st.Hosts[0].Task == nil || st.Hosts[0].Name != "PC-02" {
		t.Fatalf("host: %+v", st.Hosts[0])
	}
	if w := do(h, http.MethodDelete, path+"/task", ""); w.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodDelete, path, ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodDelete, path, ""); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", w.Code)
	}
}

func jsonInt(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestForgetDiscovered(t *testing.T) {
	h, s := panel(t)
	mac := net.HardwareAddr{0xaa, 0xbb, 0xcc, 0, 0, 1}
	if err := s.Seen(context.Background(), mac); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if w := do(h, http.MethodDelete, "/api/discovered/aa:bb:cc:00:00:01", ""); w.Code != http.StatusNoContent {
		t.Fatalf("forget: %d %s", w.Code, w.Body)
	}
	if st := getState(t, h); len(st.Discovered) != 0 {
		t.Fatalf("discovered: %+v", st.Discovered)
	}
}

func TestRejectsBadInput(t *testing.T) {
	h, _ := panel(t)
	tests := []struct {
		name, method, path, body string
		want                     int
	}{
		{"bad mac", http.MethodPost, "/api/hosts", `{"mac":"nope","name":"x"}`, http.StatusBadRequest},
		{"long mac", http.MethodPost, "/api/hosts", `{"mac":"00:00:00:00:fe:80:00:00","name":"x"}`, http.StatusBadRequest},
		{"empty name", http.MethodPost, "/api/hosts", `{"mac":"aa:bb:cc:00:00:01","name":"  "}`, http.StatusBadRequest},
		{"long name", http.MethodPost, "/api/hosts", `{"mac":"aa:bb:cc:00:00:01","name":"` + strings.Repeat("я", 65) + `"}`, http.StatusBadRequest},
		{"broken json", http.MethodPost, "/api/hosts", `{`, http.StatusBadRequest},
		{"bad id", http.MethodDelete, "/api/hosts/abc", "", http.StatusBadRequest},
		{"task for a missing host", http.MethodPost, "/api/hosts/42/task", "", http.StatusNotFound},
		{"nothing to cancel", http.MethodDelete, "/api/hosts/42/task", "", http.StatusNotFound},
		{"bad discovered mac", http.MethodDelete, "/api/discovered/nope", "", http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(h, tc.method, tc.path, tc.body); w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

// TestGuard covers attacks through the operator's own browser: the panel has
// no login, so a hostile page must not be able to drive it.
func TestGuard(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*http.Request)
		want   int
	}{
		{"same origin works", func(r *http.Request) { r.Header.Set("Origin", "http://"+addr) }, http.StatusCreated},
		{"localhost works", func(r *http.Request) { r.Host = "localhost:8081" }, http.StatusCreated},
		{"foreign host (DNS rebinding)", func(r *http.Request) { r.Host = "evil.example" }, http.StatusForbidden},
		{"foreign origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, http.StatusForbidden},
		{"form post", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, http.StatusUnsupportedMediaType},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := panel(t)
			body := `{"mac":"aa:bb:cc:00:00:01","name":"x"}`
			r := httptest.NewRequest(http.MethodPost, "/api/hosts", strings.NewReader(body))
			r.Host = addr
			r.Header.Set("Content-Type", "application/json")
			tc.mutate(r)
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

// imageEnv is a panel with one PC and a chosen image directory.
func imageEnv(t *testing.T) (http.Handler, *store.Store, string, string) {
	t.Helper()
	h, s, root := panelWithRoot(t)
	w := do(h, http.MethodPost, "/api/hosts", `{"mac":"04:7c:16:e7:d2:1e","name":"PC-01"}`)
	var host store.Host
	if err := json.Unmarshal(w.Body.Bytes(), &host); err != nil {
		t.Fatalf("decode: %v", err)
	}
	dir := root + "-images"
	if w := saveDir(h, dir); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	return h, s, dir, "/api/hosts/" + jsonInt(host.ID)
}

func TestCreateImageMakesItsFolder(t *testing.T) {
	h, s, dir, _ := imageEnv(t)
	if w := do(h, http.MethodPost, "/api/images", `{"name":" win11-club "}`); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	if info, err := os.Stat(filepath.Join(dir, "win11-club")); err != nil || !info.IsDir() {
		t.Fatalf("folder: %v", err)
	}
	if w := do(h, http.MethodPost, "/api/images", `{"name":"win11-club"}`); w.Code != http.StatusConflict {
		t.Fatalf("duplicate: %d", w.Code)
	}
	for _, name := range []string{"../evil", `a\b`, "", "русский"} {
		body, _ := json.Marshal(map[string]string{"name": name})
		if w := do(h, http.MethodPost, "/api/images", string(body)); w.Code != http.StatusBadRequest {
			t.Fatalf("%q: %d", name, w.Code)
		}
	}
	if w := do(h, http.MethodDelete, "/api/images/win11-club", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "win11-club")); !os.IsNotExist(err) {
		t.Fatalf("folder left: %v", err)
	}
	version := store.Version{Number: 1, File: "v1.ldisk", Size: 1, SourceMAC: "aa"}
	if err := s.AddVersion(context.Background(), "kept", version); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if w := do(h, http.MethodDelete, "/api/images/kept", ""); w.Code != http.StatusConflict {
		t.Fatalf("delete with versions: %d", w.Code)
	}
}

func TestCreateImageNeedsADirectory(t *testing.T) {
	h, _ := panel(t)
	if w := do(h, http.MethodPost, "/api/images", `{"name":"win11"}`); w.Code != http.StatusConflict {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func TestTasksUseTheImageOfThePC(t *testing.T) {
	h, s, dir, path := imageEnv(t)
	ctx := context.Background()

	if w := do(h, http.MethodPost, path+"/task", `{"kind":"clone"}`); w.Code != http.StatusConflict {
		t.Fatalf("no image chosen: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, "/api/images", `{"name":"win11"}`); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d", w.Code)
	}
	if w := do(h, http.MethodPut, path+"/image", `{"image":"nope"}`); w.Code != http.StatusNotFound {
		t.Fatalf("missing image: %d", w.Code)
	}
	if w := do(h, http.MethodPut, path+"/image", `{"image":"win11"}`); w.Code != http.StatusNoContent {
		t.Fatalf("link: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"deploy"}`); w.Code != http.StatusConflict {
		t.Fatalf("deploy of an empty image: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"wipe"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown kind: %d", w.Code)
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"clone"}`); w.Code != http.StatusCreated {
		t.Fatalf("clone: %d %s", w.Code, w.Body)
	}
	hosts, _ := s.Hosts(ctx)
	if tk := hosts[0].Task; tk == nil || tk.Kind != store.TaskClone || tk.Image != "win11" {
		t.Fatalf("task: %+v", tk)
	}
	if w := do(h, http.MethodDelete, path+"/task", ""); w.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", w.Code)
	}

	if err := os.WriteFile(filepath.Join(dir, "win11", "v1.ldisk"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	version := store.Version{Number: 1, File: "v1.ldisk", Size: 1, SourceMAC: "aa"}
	if err := s.AddVersion(ctx, "win11", version); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"deploy"}`); w.Code != http.StatusCreated {
		t.Fatalf("deploy: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodDelete, path+"/task", ""); w.Code != http.StatusNoContent {
		t.Fatalf("cancel: %d", w.Code)
	}
	if w := do(h, http.MethodPost, path+"/task", ""); w.Code != http.StatusCreated {
		t.Fatalf("link check needs no image: %d %s", w.Code, w.Body)
	}
}

func TestExitModeSetting(t *testing.T) {
	h, s, _ := panelWithRoot(t)
	var got struct{ ExitMode string }
	w := do(h, http.MethodGet, "/api/settings", "")
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || got.ExitMode != "sanboot" {
		t.Fatalf("default: %+v, err %v", got, err)
	}
	if w := do(h, http.MethodPut, "/api/settings", `{"exitMode":"refind"}`); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if v, _ := s.Setting(context.Background(), store.SettingExitMode); v != "refind" {
		t.Fatalf("stored: %q", v)
	}
	if w := do(h, http.MethodPut, "/api/settings", `{"exitMode":"reboot"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown mode: %d", w.Code)
	}
}

func TestDeleteOnlyTheNewestVersion(t *testing.T) {
	h, s, dir, _ := imageEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, "win11"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	for i, name := range []string{"v1.ldisk", "v2.ldelta"} {
		if err := os.WriteFile(filepath.Join(dir, "win11", name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := s.AddVersion(ctx, "win11", store.Version{Number: i + 1, File: name, Size: 1, SourceMAC: "aa", Parent: i}); err != nil {
			t.Fatalf("AddVersion: %v", err)
		}
	}
	if w := do(h, http.MethodDelete, "/api/images/win11/versions/1", ""); w.Code != http.StatusConflict {
		t.Fatalf("a parent was deleted: %d", w.Code)
	}
	if w := do(h, http.MethodDelete, "/api/images/win11/versions/2", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(dir, "win11", "v2.ldelta")); !os.IsNotExist(err) {
		t.Fatalf("file left: %v", err)
	}
	if latest, _ := s.LatestVersion(ctx, "win11"); latest == nil || latest.Number != 1 {
		t.Fatalf("latest: %+v", latest)
	}
	if w := do(h, http.MethodDelete, "/api/images/win11/versions/1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete the last one: %d %s", w.Code, w.Body)
	}
	if latest, _ := s.LatestVersion(ctx, "win11"); latest != nil {
		t.Fatalf("latest: %+v", latest)
	}
	if w := do(h, http.MethodDelete, "/api/images/win11", ""); w.Code != http.StatusNoContent {
		t.Fatalf("the image is empty now: %d %s", w.Code, w.Body)
	}
}

// TestImageInUseIsProtected: a queued task counts, its PC can boot at any moment.
func TestImageInUseIsProtected(t *testing.T) {
	h, s, dir, path := imageEnv(t)
	ctx := context.Background()
	if err := os.MkdirAll(filepath.Join(dir, "win11"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	file := filepath.Join(dir, "win11", "v1.ldisk")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	version := store.Version{Number: 1, File: "v1.ldisk", Size: 1, SourceMAC: "aa"}
	if err := s.AddVersion(ctx, "win11", version); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "empty"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := s.CreateImage(ctx, "empty", filepath.Join(dir, "empty")); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}

	for _, image := range []string{"win11", "empty"} {
		if w := do(h, http.MethodPut, path+"/image", `{"image":"`+image+`"}`); w.Code != http.StatusNoContent {
			t.Fatalf("link: %d", w.Code)
		}
		if w := do(h, http.MethodPost, path+"/task", `{"kind":"clone"}`); w.Code != http.StatusCreated {
			t.Fatalf("clone: %d %s", w.Code, w.Body)
		}
		if image == "win11" {
			if w := do(h, http.MethodDelete, "/api/images/win11/versions/1", ""); w.Code != http.StatusConflict {
				t.Fatalf("version of a busy image: %d", w.Code)
			}
			if _, err := os.Stat(file); err != nil {
				t.Fatalf("the file of a busy image was removed: %v", err)
			}
		} else if w := do(h, http.MethodDelete, "/api/images/empty", ""); w.Code != http.StatusConflict {
			t.Fatalf("busy empty image: %d", w.Code)
		}
		st := getState(t, h)
		if len(st.Images) != 2 {
			t.Fatalf("images: %+v", st.Images)
		}
		busy := 0
		for _, i := range st.Images {
			busy += i.Busy
		}
		if busy != 1 {
			t.Fatalf("busy: %+v", st.Images)
		}
		if w := do(h, http.MethodDelete, path+"/task", ""); w.Code != http.StatusNoContent {
			t.Fatalf("cancel: %d", w.Code)
		}
	}

	if w := do(h, http.MethodDelete, "/api/images/win11/versions/1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("delete after the task ended: %d %s", w.Code, w.Body)
	}
}

// TestChangingTheImageDirKeepsImages guards the links of every PC: an image stays
// where it was created, and only new images go to the new directory.
func TestChangingTheImageDirKeepsImages(t *testing.T) {
	h, dir, path := missingEnv(t)
	other := dir + "-other"
	if w := saveDir(h, other); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	st := getState(t, h)
	if len(st.Images) != 1 || st.Images[0].Missing || st.Images[0].Path != filepath.Join(dir, "win11") {
		t.Fatalf("images: %+v", st.Images)
	}
	if st.Hosts[0].Image != "win11" {
		t.Fatalf("the PC lost its image: %+v", st.Hosts[0])
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"deploy"}`); w.Code != http.StatusCreated {
		t.Fatalf("deploy: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, "/api/images", `{"name":"win12"}`); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(other, "win12")); err != nil {
		t.Fatalf("a new image went elsewhere: %v", err)
	}
}

// TestImageOnAnUnreachableDiskKeepsItsLinks is the same for one image: its disk may
// come back, so the PC keeps it, and only tasks are refused.
func TestImageOnAnUnreachableDiskKeepsItsLinks(t *testing.T) {
	h, dir, path := missingEnv(t)
	other := dir + "-other"
	if w := saveDir(h, other); w.Code != http.StatusOK {
		t.Fatalf("save: %d %s", w.Code, w.Body)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	st := getState(t, h)
	if !st.Images[0].Offline || st.Images[0].Missing || st.Hosts[0].Image != "win11" {
		t.Fatalf("state: %+v %+v", st.Images, st.Hosts)
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"deploy"}`); w.Code != http.StatusConflict {
		t.Fatalf("deploy from an unreachable disk: %d", w.Code)
	}
}

// missingEnv is a PC linked to an image with one version on disk.
func missingEnv(t *testing.T) (http.Handler, string, string) {
	t.Helper()
	h, s, dir, path := imageEnv(t)
	if w := do(h, http.MethodPost, "/api/images", `{"name":"win11"}`); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d", w.Code)
	}
	file := filepath.Join(dir, "win11", "v1.ldisk")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	version := store.Version{Number: 1, File: "v1.ldisk", Size: 1, SourceMAC: "aa"}
	if err := s.AddVersion(context.Background(), "win11", version); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if w := do(h, http.MethodPut, path+"/image", `{"image":"win11"}`); w.Code != http.StatusNoContent {
		t.Fatalf("link: %d", w.Code)
	}
	return h, dir, path
}

func TestMissingFileIsMarkedAndUnlinked(t *testing.T) {
	h, dir, path := missingEnv(t)
	if st := getState(t, h); st.Images[0].Missing || st.Hosts[0].Image != "win11" {
		t.Fatalf("state: %+v", st)
	}

	if err := os.Remove(filepath.Join(dir, "win11", "v1.ldisk")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	st := getState(t, h)
	if !st.Images[0].Missing || !st.Images[0].Versions[0].Missing {
		t.Fatalf("images: %+v", st.Images)
	}
	if st.Hosts[0].Image != "" {
		t.Fatalf("PC kept a missing image: %+v", st.Hosts[0])
	}
	if st := getState(t, h); st.Hosts[0].Image != "" {
		t.Fatalf("the link was not removed for good: %+v", st.Hosts[0])
	}
	if w := do(h, http.MethodPut, path+"/image", `{"image":"win11"}`); w.Code != http.StatusNoContent {
		t.Fatalf("link: %d", w.Code)
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"deploy"}`); w.Code != http.StatusConflict {
		t.Fatalf("deploy of a missing image: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodDelete, "/api/images/win11/versions/1", ""); w.Code != http.StatusNoContent {
		t.Fatalf("a lost version must be removable: %d %s", w.Code, w.Body)
	}
}

func TestLostFolderIsMissingEvenWithoutVersions(t *testing.T) {
	h, _, dir, _ := imageEnv(t)
	if w := do(h, http.MethodPost, "/api/images", `{"name":"empty"}`); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d", w.Code)
	}
	if err := os.Remove(filepath.Join(dir, "empty")); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if st := getState(t, h); !st.Images[0].Missing {
		t.Fatalf("images: %+v", st.Images)
	}
}

// TestOfflineStorageKeepsTheLinks: a disconnected drive is not a deleted image.
func TestOfflineStorageKeepsTheLinks(t *testing.T) {
	h, dir, path := missingEnv(t)
	moved := dir + "-unplugged"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	st := getState(t, h)
	if !st.StorageOffline || st.Hosts[0].Image != "win11" || st.Images[0].Missing {
		t.Fatalf("state: %+v", st)
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"clone"}`); w.Code != http.StatusConflict {
		t.Fatalf("task without storage: %d %s", w.Code, w.Body)
	}
	if err := os.Rename(moved, dir); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if st := getState(t, h); st.StorageOffline || st.Hosts[0].Image != "win11" {
		t.Fatalf("state after reconnect: %+v", st)
	}
}

// TestHostCanBeAddedWithAnImage: the image is optional when a PC is added.
func TestHostCanBeAddedWithAnImage(t *testing.T) {
	h, _, _, _ := imageEnv(t)
	if w := do(h, http.MethodPost, "/api/images", `{"name":"win11"}`); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d", w.Code)
	}
	w := do(h, http.MethodPost, "/api/hosts", `{"mac":"04:7c:16:e7:d2:2a","name":"PC-02","image":"win11"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("add: %d %s", w.Code, w.Body)
	}
	w = do(h, http.MethodPost, "/api/hosts", `{"mac":"04:7c:16:e7:d2:2b","name":"PC-03","image":"nope"}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("unknown image: %d", w.Code)
	}
	st := getState(t, h)
	if len(st.Hosts) != 2 || st.Hosts[1].Name != "PC-02" || st.Hosts[1].Image != "win11" {
		t.Fatalf("hosts: %+v", st.Hosts)
	}
}

// TestStateListsTasks: active tasks come first, then the finished ones, newest first.
func TestStateListsTasks(t *testing.T) {
	h, s, _, path := imageEnv(t)
	ctx := context.Background()
	for range 2 {
		if w := do(h, http.MethodPost, path+"/task", `{"kind":"agent"}`); w.Code != http.StatusCreated {
			t.Fatalf("task: %d %s", w.Code, w.Body)
		}
		if w := do(h, http.MethodDelete, path+"/task", ""); w.Code != http.StatusNoContent {
			t.Fatalf("cancel: %d", w.Code)
		}
	}
	if w := do(h, http.MethodPost, path+"/task", `{"kind":"agent"}`); w.Code != http.StatusCreated {
		t.Fatalf("task: %d %s", w.Code, w.Body)
	}
	tasks, err := s.RecentTasks(ctx, 10)
	if err != nil {
		t.Fatalf("RecentTasks: %v", err)
	}
	if len(tasks) != 3 || tasks[0].FinishedAt != nil || tasks[1].ID < tasks[2].ID || tasks[0].Host != "PC-01" {
		t.Fatalf("tasks: %+v", tasks)
	}
}

// TestBrokenChainIsRefusedByThePanel: files that are not a chain change nothing. The
// chain itself is tested in imaging.
func TestBrokenChainIsRefusedByThePanel(t *testing.T) {
	h, s, dir, _ := imageEnv(t)
	ctx := context.Background()
	if w := do(h, http.MethodPost, "/api/images", `{"name":"win11"}`); w.Code != http.StatusNoContent {
		t.Fatalf("create: %d", w.Code)
	}
	for i, name := range []string{"v1.ldisk", "v2.ldelta"} {
		if err := os.WriteFile(filepath.Join(dir, "win11", name), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		v := store.Version{Number: i + 1, File: name, Size: 1, SourceMAC: "aa", Parent: i}
		if err := s.AddVersion(ctx, "win11", v); err != nil {
			t.Fatalf("AddVersion: %v", err)
		}
	}
	if w := do(h, http.MethodPost, "/api/images/win11/versions/1/restore", ""); w.Code != http.StatusConflict {
		t.Fatalf("restore: %d %s", w.Code, w.Body)
	}
	if w := do(h, http.MethodPost, "/api/images/win11/versions/9/restore", ""); w.Code != http.StatusNotFound {
		t.Fatalf("unknown version: %d", w.Code)
	}
	if w := do(h, http.MethodDelete, "/api/images/win11/versions/1", ""); w.Code != http.StatusConflict {
		t.Fatalf("delete of a parent: %d", w.Code)
	}
	if st := getState(t, h); len(st.Images[0].Versions) != 2 || st.Images[0].Versions[1].Disabled {
		t.Fatalf("versions: %+v", st.Images[0].Versions)
	}
}
