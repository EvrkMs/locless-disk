package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"time"
)

// Version is one file of an image chain.
type Version struct {
	Number    int       `json:"number"`
	File      string    `json:"file"` // relative to the directory of the image
	Size      int64     `json:"size"`
	SourceMAC string    `json:"sourceMac"`
	CreatedAt time.Time `json:"createdAt"`
	Missing   bool      `json:"missing"`  // the file is gone; set by whoever looked at the disk
	Parent    int       `json:"parent"`   // the version it builds on, 0 for an .ldisk
	Disabled  bool      `json:"disabled"` // switched off by a restore; the file stays
}

// Image is a named chain of versions, oldest first.
type Image struct {
	ID       int64     `json:"id"`
	Name     string    `json:"name"`
	Path     string    `json:"path"` // full path of its folder, fixed when it was created
	Versions []Version `json:"versions"`
	Busy     int       `json:"busy"`    // unfinished tasks using the image
	Missing  bool      `json:"missing"` // its folder or a version file is gone
	Offline  bool      `json:"offline"` // the place its folder lives in is unreachable
	Merging  bool      `json:"merging"` // its versions are being merged right now
	MergeErr string    `json:"mergeError"`
}

// ErrImageExists means the name is taken.
var ErrImageExists = errors.New("store: image already exists")

// ErrImageInUse means the image has versions, which must not vanish by accident.
var ErrImageInUse = errors.New("store: image has versions")

// CreateImage adds an empty image that lives in the folder at path. The path stays
// with the image: changing the image directory later only affects new images.
func (s *Store) CreateImage(ctx context.Context, name, path string) error {
	res, err := s.db.ExecContext(ctx, "INSERT INTO images(name, path, created_at) VALUES(?, ?, ?) ON CONFLICT(name) DO NOTHING", name, path, unix(time.Now()))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrImageExists
	}
	return nil
}

// ImagePath returns the folder of an image. An image not created yet would go
// into the image directory; "" means no directory has been chosen.
func (s *Store) ImagePath(ctx context.Context, name string) (string, error) {
	var path string
	err := s.db.QueryRowContext(ctx, "SELECT path FROM images WHERE name = ?", name).Scan(&path)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if path != "" {
		return path, nil
	}
	dir, err := s.Setting(ctx, SettingImageDir)
	if err != nil || dir == "" {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// fillImagePaths gives images from before paths were stored the place they are in
// now, while the image directory still says where that is.
func (s *Store) fillImagePaths(ctx context.Context) error {
	dir, err := s.Setting(ctx, SettingImageDir)
	if err != nil || dir == "" {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT name FROM images WHERE path = ''")
	if err != nil {
		return err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// The pool has one connection, so the rows must be closed before the next query.
	rows.Close()
	for _, name := range names {
		if _, err := s.db.ExecContext(ctx, "UPDATE images SET path = ? WHERE name = ?", filepath.Join(dir, name), name); err != nil {
			return err
		}
	}
	return nil
}

// DeleteImage removes an image without versions; PCs that used it lose the link.
func (s *Store) DeleteImage(ctx context.Context, name string) error {
	var versions int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM image_versions v JOIN images i ON i.id = v.image_id WHERE i.name = ?`, name).Scan(&versions)
	if err != nil {
		return err
	}
	if versions > 0 {
		return ErrImageInUse
	}
	res, err := s.db.ExecContext(ctx, "DELETE FROM images WHERE name = ?", name)
	return affected(res, err)
}

// SetHostImage links a PC to an image; an empty name removes the link.
func (s *Store) SetHostImage(ctx context.Context, hostID int64, image string) error {
	if image == "" {
		res, err := s.db.ExecContext(ctx, "UPDATE hosts SET image_id = NULL WHERE id = ?", hostID)
		return affected(res, err)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE hosts SET image_id = (SELECT id FROM images WHERE name = ?1)
		WHERE id = ?2 AND EXISTS(SELECT 1 FROM images WHERE name = ?1)`, image, hostID)
	return affected(res, err)
}

// ClearImageLinks unlinks every PC from an image.
func (s *Store) ClearImageLinks(ctx context.Context, image string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE hosts SET image_id = NULL WHERE image_id = (SELECT id FROM images WHERE name = ?)`, image)
	return err
}

// HostImage returns the image a PC belongs to, or "" if it has none.
func (s *Store) HostImage(ctx context.Context, hostID int64) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(i.name, '') FROM hosts h LEFT JOIN images i ON i.id = h.image_id WHERE h.id = ?`, hostID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// LatestVersion returns the current version of an image, or nil if it has none.
func (s *Store) LatestVersion(ctx context.Context, image string) (*Version, error) {
	var v Version
	var created int64
	err := s.db.QueryRowContext(ctx, `
		SELECT v.number, v.file, v.size, v.source_mac, v.created_at, v.parent
		FROM image_versions v JOIN images i ON i.id = v.image_id
		WHERE i.name = ? AND v.disabled = 0 ORDER BY v.number DESC LIMIT 1`, image).Scan(&v.Number, &v.File, &v.Size, &v.SourceMAC, &created, &v.Parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v.CreatedAt = fromUnix(created)
	return &v, nil
}

// Versions lists the chain in use, oldest first: what a deploy writes.
func (s *Store) Versions(ctx context.Context, image string) ([]Version, error) {
	return s.versions(ctx, image, "AND v.disabled = 0")
}

// AllVersions lists every version, the switched off ones too, oldest first.
func (s *Store) AllVersions(ctx context.Context, image string) ([]Version, error) {
	return s.versions(ctx, image, "")
}

func (s *Store) versions(ctx context.Context, image, filter string) ([]Version, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT v.number, v.file, v.size, v.source_mac, v.created_at, v.parent, v.disabled
		FROM image_versions v JOIN images i ON i.id = v.image_id
		WHERE i.name = ? `+filter+` ORDER BY v.number`, image)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	versions := []Version{}
	for rows.Next() {
		var v Version
		var created int64
		if err := rows.Scan(&v.Number, &v.File, &v.Size, &v.SourceMAC, &created, &v.Parent, &v.Disabled); err != nil {
			return nil, err
		}
		v.CreatedAt = fromUnix(created)
		versions = append(versions, v)
	}
	return versions, rows.Err()
}

// NextNumber returns the number of the next version. Switched off versions count:
// their files keep their names.
func (s *Store) NextNumber(ctx context.Context, image string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(v.number), 0) + 1
		FROM image_versions v JOIN images i ON i.id = v.image_id WHERE i.name = ?`, image).Scan(&n)
	return n, err
}

// DisableAfter switches off the versions of the chain after number.
func (s *Store) DisableAfter(ctx context.Context, image string, number int) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE image_versions SET disabled = 1
		WHERE number > ? AND image_id = (SELECT id FROM images WHERE name = ?)`, number, image)
	return err
}

// EnableVersions switches versions back on.
func (s *Store) EnableVersions(ctx context.Context, image string, numbers []int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, n := range numbers {
		_, err := tx.ExecContext(ctx, `
			UPDATE image_versions SET disabled = 0
			WHERE number = ? AND image_id = (SELECT id FROM images WHERE name = ?)`, n, image)
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ReplaceVersions makes v the only version of an image.
func (s *Store) ReplaceVersions(ctx context.Context, image string, v Version) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, "DELETE FROM image_versions WHERE image_id = (SELECT id FROM images WHERE name = ?)", image)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO image_versions(image_id, number, file, size, source_mac, created_at, parent)
		VALUES((SELECT id FROM images WHERE name = ?), ?, ?, ?, ?, ?, 0)`,
		image, v.Number, v.File, v.Size, v.SourceMAC, unix(time.Now()))
	if err != nil {
		return err
	}
	return tx.Commit()
}

// ActiveImageTasks counts unfinished tasks that read or write an image; with an
// empty name it counts them for all images. A queued task counts: its PC may boot
// at any moment, and the files must still be there.
func (s *Store) ActiveImageTasks(ctx context.Context, image string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tasks
		WHERE finished_at IS NULL AND image != '' AND (?1 = '' OR image = ?1)`, image).Scan(&n)
	return n, err
}

// DeleteVersion forgets one version of an image.
func (s *Store) DeleteVersion(ctx context.Context, image string, number int) error {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM image_versions
		WHERE number = ? AND image_id = (SELECT id FROM images WHERE name = ?)`, number, image)
	return affected(res, err)
}

// AddVersion records a finished version, creating the image if it is new.
func (s *Store) AddVersion(ctx context.Context, image string, v Version) error {
	path, err := s.ImagePath(ctx, image)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := unix(time.Now())
	_, err = tx.ExecContext(ctx, "INSERT INTO images(name, path, created_at) VALUES(?, ?, ?) ON CONFLICT(name) DO NOTHING", image, path, now)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO image_versions(image_id, number, file, size, source_mac, created_at, parent)
		VALUES((SELECT id FROM images WHERE name = ?), ?, ?, ?, ?, ?, ?)`,
		image, v.Number, v.File, v.Size, v.SourceMAC, now, v.Parent)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// Images lists images by name with their versions.
func (s *Store) Images(ctx context.Context) ([]Image, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT i.id, i.name, i.path, v.number, v.file, v.size, v.source_mac, v.created_at, v.parent, v.disabled
		FROM images i LEFT JOIN image_versions v ON v.image_id = i.id
		ORDER BY i.name COLLATE NOCASE, v.number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	images := []Image{}
	for rows.Next() {
		var id int64
		var name, path string
		var number, size, created, parent sql.NullInt64
		var disabled sql.NullBool
		var file, mac sql.NullString
		if err := rows.Scan(&id, &name, &path, &number, &file, &size, &mac, &created, &parent, &disabled); err != nil {
			return nil, err
		}
		if len(images) == 0 || images[len(images)-1].ID != id {
			images = append(images, Image{ID: id, Name: name, Path: path, Versions: []Version{}})
		}
		if number.Valid {
			last := &images[len(images)-1]
			last.Versions = append(last.Versions, Version{
				Number: int(number.Int64), File: file.String, Size: size.Int64,
				SourceMAC: mac.String, CreatedAt: fromUnix(created.Int64),
				Parent: int(parent.Int64), Disabled: disabled.Bool,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The pool has one connection, so the rows must be closed before the next query.
	rows.Close()
	for i := range images {
		if images[i].Busy, err = s.ActiveImageTasks(ctx, images[i].Name); err != nil {
			return nil, err
		}
	}
	return images, nil
}
