package web

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"fogs/server/store"
)

func (h *server) settings(w http.ResponseWriter, r *http.Request) {
	dir, err := h.store.Setting(r.Context(), store.SettingImageDir)
	if err != nil {
		h.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"imageDir": dir, "root": h.root})
}

func (h *server) saveSettings(w http.ResponseWriter, r *http.Request) {
	var in struct{ ImageDir string }
	if !readJSON(w, r, &in) {
		return
	}
	dir, err := prepareImageDir(strings.TrimSpace(in.ImageDir), h.root)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.store.SetSetting(r.Context(), store.SettingImageDir, dir); err != nil {
		h.fail(w, err)
		return
	}
	h.log.Info("image directory changed", "dir", dir)
	writeJSON(w, http.StatusOK, map[string]string{"imageDir": dir, "root": h.root})
}

// prepareImageDir validates the directory for disk images, creates it if needed
// and returns its clean path. The errors are shown to the operator as they are.
func prepareImageDir(dir, root string) (string, error) {
	if dir == "" {
		return "", errors.New("Укажите папку")
	}
	if !filepath.IsAbs(dir) {
		return "", errors.New(`Нужен полный путь, например D:\fogs-images`)
	}
	dir = filepath.Clean(dir)
	// Check before creating anything: a refused path must leave no trace.
	inside, err := isInside(dir, root)
	if err != nil {
		return "", errors.New("Не удалось проверить папку")
	}
	if inside {
		return "", errors.New("Папка образов не может быть внутри каталога загрузки: оттуда файлы доступны всем в сети без пароля")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", errors.New("Не удалось создать папку: проверьте путь и права")
	}
	probe, err := os.CreateTemp(dir, ".fogs-check-*")
	if err != nil {
		return "", errors.New("В папку нельзя записывать: проверьте права")
	}
	probe.Close()
	os.Remove(probe.Name())
	return dir, nil
}

// isInside reports whether dir is root or lies below it, following links so
// that one cannot lead images into the served directory.
func isInside(dir, root string) (bool, error) {
	d, err := resolve(dir)
	if err != nil {
		return false, err
	}
	r, err := resolve(root)
	if err != nil {
		return false, err
	}
	rel, err := filepath.Rel(r, d)
	if err != nil {
		return false, nil // on different drives
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)), nil
}

// resolve follows links in the part of the path that exists; the rest is kept
// as it is, because the directory may not have been created yet.
func resolve(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rest := ""
	for cur := abs; ; {
		real, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(real, rest), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return abs, nil
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}
