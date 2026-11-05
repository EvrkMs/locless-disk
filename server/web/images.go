package web

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"fogs/proto"
	"fogs/server/imaging"
	"fogs/server/store"
)

// createImage adds an image and its folder inside the image directory.
func (h *server) createImage(w http.ResponseWriter, r *http.Request) {
	var in struct{ Name string }
	if !readJSON(w, r, &in) {
		return
	}
	name := strings.TrimSpace(in.Name)
	if !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Имя образа: латиница, цифры, - и _, до 64 символов")
		return
	}
	dir, err := h.store.Setting(r.Context(), store.SettingImageDir)
	if err != nil {
		h.fail(w, err)
		return
	}
	if dir == "" {
		writeError(w, http.StatusConflict, "Сначала укажите папку образов в настройках")
		return
	}
	folder := filepath.Join(dir, name)
	if err := h.store.CreateImage(r.Context(), name, folder); err != nil {
		if errors.Is(err, store.ErrImageExists) {
			writeError(w, http.StatusConflict, "Образ с таким именем уже есть")
			return
		}
		h.fail(w, err)
		return
	}
	if err := os.MkdirAll(folder, 0o755); err != nil {
		h.store.DeleteImage(r.Context(), name)
		writeError(w, http.StatusInternalServerError, "Не удалось создать папку образа")
		return
	}
	h.log.Info("image created", "name", name)
	w.WriteHeader(http.StatusNoContent)
}

// deleteImage removes an empty image. Versions are never deleted from here.
func (h *server) deleteImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Неверное имя образа")
		return
	}
	if h.imageBusy(w, r, name) {
		return
	}
	folder, err := h.store.ImagePath(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return
	}
	err = h.store.DeleteImage(r.Context(), name)
	if errors.Is(err, store.ErrImageInUse) {
		writeError(w, http.StatusConflict, "В образе есть версии, его нельзя удалить")
		return
	}
	if err == nil && folder != "" {
		os.Remove(folder) // an empty folder is all there is; it stays if something else lives in it
	}
	h.done(w, err)
}

// deleteVersion removes a version nothing builds on: the newest one, or a switched
// off one at the end of its line. Every other one is a parent later versions need.
func (h *server) deleteVersion(w http.ResponseWriter, r *http.Request) {
	name, number, ok := versionPath(w, r)
	if !ok || h.imageBusy(w, r, name) {
		return
	}
	versions, err := h.store.AllVersions(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return
	}
	i := slices.IndexFunc(versions, func(v store.Version) bool { return v.Number == number })
	if i < 0 {
		writeError(w, http.StatusNotFound, "Не найдено")
		return
	}
	if slices.ContainsFunc(versions, func(v store.Version) bool { return v.Parent == number }) {
		writeError(w, http.StatusConflict, "Удалить можно только последнюю версию: от остальных зависят следующие")
		return
	}
	folder, err := h.store.ImagePath(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return
	}
	if err := os.Remove(filepath.Join(folder, versions[i].File)); err != nil && !errors.Is(err, os.ErrNotExist) {
		writeError(w, http.StatusConflict, "Файл занят: возможно, идёт развёртывание этой версии")
		return
	}
	h.log.Info("version deleted", "image", name, "file", versions[i].File)
	h.done(w, h.store.DeleteVersion(r.Context(), name, number))
}

// versionPath reads the image and the version of a request.
func versionPath(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	name := r.PathValue("name")
	number, err := strconv.Atoi(r.PathValue("number"))
	if err != nil || !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Неверная версия")
		return "", 0, false
	}
	return name, number, true
}

// restoreVersion makes an older version the current one; later ones are kept.
func (h *server) restoreVersion(w http.ResponseWriter, r *http.Request) {
	name, number, ok := versionPath(w, r)
	if ok && !h.imageBusy(w, r, name) {
		h.versionResult(w, h.images.Restore(r.Context(), name, number))
	}
}

// enableVersion brings a switched off version back.
func (h *server) enableVersion(w http.ResponseWriter, r *http.Request) {
	name, number, ok := versionPath(w, r)
	if ok && !h.imageBusy(w, r, name) {
		h.versionResult(w, h.images.Enable(r.Context(), name, number))
	}
}

// mergeImage starts folding the chain of an image into one file.
func (h *server) mergeImage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !proto.ValidImageName(name) {
		writeError(w, http.StatusBadRequest, "Неверное имя образа")
		return
	}
	if !h.imageBusy(w, r, name) {
		h.versionResult(w, h.images.StartMerge(r.Context(), name))
	}
}

// versionResult tells the operator why the versions of an image did not change.
func (h *server) versionResult(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, imaging.ErrForked):
		writeError(w, http.StatusConflict, "Вернуть нельзя: после отката снят новый образ, цепочка пошла другим путём")
	case errors.Is(err, imaging.ErrBrokenChain):
		h.log.Warn("broken chain", "err", err)
		writeError(w, http.StatusConflict, "Цепочка версий нарушена: файлы не подходят друг к другу или пропали")
	case errors.Is(err, imaging.ErrNothingToMerge):
		writeError(w, http.StatusConflict, "Сливать нечего: в цепочке одна версия")
	case errors.Is(err, imaging.ErrBusy):
		writeError(w, http.StatusConflict, "Образ сейчас занят")
	default:
		h.done(w, err)
	}
}

// imageBusy answers the request itself if a task uses the image: a clone builds
// on its newest version, and a deploy reads its files.
func (h *server) imageBusy(w http.ResponseWriter, r *http.Request, name string) bool {
	if merging, _ := h.images.Merging(name); merging {
		writeError(w, http.StatusConflict, "Идёт слияние версий образа, дождитесь окончания")
		return true
	}
	n, err := h.store.ActiveImageTasks(r.Context(), name)
	if err != nil {
		h.fail(w, err)
		return true
	}
	if n > 0 {
		writeError(w, http.StatusConflict, "Образ используется задачей: дождитесь её окончания или отмените")
		return true
	}
	return false
}

// setHostImage links a PC to an image, or clears the link.
func (h *server) setHostImage(w http.ResponseWriter, r *http.Request) {
	id, ok := hostID(w, r)
	if !ok {
		return
	}
	var in struct{ Image string }
	if !readJSON(w, r, &in) {
		return
	}
	h.done(w, h.store.SetHostImage(r.Context(), id, in.Image))
}

// imageReady answers the request itself if the image cannot serve a task: its
// files are gone, or a deploy has no version to write.
func (h *server) imageReady(w http.ResponseWriter, r *http.Request, name string, needVersions bool) bool {
	images, err := h.store.Images(r.Context())
	if err != nil {
		h.fail(w, err)
		return false
	}
	if _, err := h.auditImages(r.Context(), images, nil); err != nil {
		h.fail(w, err)
		return false
	}
	i := slices.IndexFunc(images, func(i store.Image) bool { return i.Name == name })
	switch {
	case i >= 0 && images[i].Merging:
		writeError(w, http.StatusConflict, "Идёт слияние версий образа, дождитесь окончания")
	case i >= 0 && images[i].Offline:
		writeError(w, http.StatusConflict, "Папка образа недоступна: "+images[i].Path)
	case i < 0 || images[i].Missing:
		writeError(w, http.StatusConflict, "Файлы образа не найдены")
	case needVersions && !slices.ContainsFunc(images[i].Versions, func(v store.Version) bool { return !v.Disabled }):
		writeError(w, http.StatusConflict, "В образе ещё нет версий, сначала клонируйте эталон")
	default:
		return true
	}
	return false
}

// auditImages compares the images with the disk: it marks what is gone and takes
// such images away from the PCs that used them. Every image is checked at its own
// path. If the place that holds its folder is unreachable, the image is only marked
// offline: a disk that is merely not connected must not cost the PCs their image.
// It returns true if anything is offline, the image directory included.
func (h *server) auditImages(ctx context.Context, images []store.Image, hosts []store.Host) (bool, error) {
	dir, err := h.store.Setting(ctx, store.SettingImageDir)
	if err != nil {
		return false, err
	}
	offline := dir != "" && !isDir(dir)
	for i := range images {
		image := &images[i]
		image.Merging, image.MergeErr = h.images.Merging(image.Name)
		if image.Path == "" {
			continue
		}
		if !isDir(filepath.Dir(image.Path)) {
			image.Offline, offline = true, true
			continue
		}
		image.Missing = !isDir(image.Path)
		for j := range image.Versions {
			v := &image.Versions[j]
			if _, err := os.Stat(filepath.Join(image.Path, v.File)); err != nil {
				v.Missing = true
				image.Missing = image.Missing || !v.Disabled // a switched off version harms nothing
			}
		}
		if !image.Missing {
			continue
		}
		if err := h.store.ClearImageLinks(ctx, image.Name); err != nil {
			return false, err
		}
		for k := range hosts {
			if hosts[k].Image == image.Name {
				hosts[k].Image = ""
			}
		}
	}
	return offline, nil
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
