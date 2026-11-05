// Package store keeps the server state in a SQLite file: registered hosts,
// PCs seen on the network, and tasks.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"time"

	"fogs/proto"

	_ "modernc.org/sqlite" // registers the "sqlite" driver, pure Go
)

// Errors callers can tell apart.
var (
	ErrNotFound   = errors.New("store: not found")
	ErrExists     = errors.New("store: host already exists")
	ErrTaskExists = errors.New("store: host already has a task")
)

// Task kinds.
const (
	// TaskAgent boots WinPE with the agent; it does not touch the disk.
	TaskAgent = proto.TaskKindCheck
	// TaskClone reads the disk of the PC into a new version of an image.
	TaskClone = proto.TaskKindClone
	// TaskDeploy writes an image over the disk of the PC.
	TaskDeploy = proto.TaskKindDeploy
	// TaskFormat wipes the disk of the PC into one empty volume.
	TaskFormat = proto.TaskKindFormat
)

// Task statuses.
const (
	StatusQueued    = "queued"    // waits for the PC to boot from the network
	StatusStarted   = "started"   // the PC was sent into WinPE
	StatusRunning   = "running"   // the agent is reporting
	StatusDone      = "done"      // the agent finished it
	StatusFailed    = "failed"    // the agent gave up
	StatusCancelled = "cancelled" // removed by the operator
)

// migrations are applied in order; the index plus one is the schema version.
// Never edit one that has shipped, add another.
var migrations = []string{`
	CREATE TABLE hosts (
		id         INTEGER PRIMARY KEY,
		mac        TEXT NOT NULL UNIQUE,
		name       TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		last_seen  INTEGER
	);
	CREATE TABLE discovered (
		mac        TEXT PRIMARY KEY,
		first_seen INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL
	);
	CREATE TABLE tasks (
		id          INTEGER PRIMARY KEY,
		host_id     INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
		kind        TEXT NOT NULL,
		status      TEXT NOT NULL,
		created_at  INTEGER NOT NULL,
		started_at  INTEGER,
		finished_at INTEGER
	);
	CREATE UNIQUE INDEX one_active_task ON tasks(host_id) WHERE finished_at IS NULL;
`, `
	CREATE TABLE settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);
`, `
	ALTER TABLE tasks ADD COLUMN percent INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE tasks ADD COLUMN stage TEXT NOT NULL DEFAULT '';
	ALTER TABLE tasks ADD COLUMN message TEXT NOT NULL DEFAULT '';
`, `
	ALTER TABLE tasks ADD COLUMN image TEXT NOT NULL DEFAULT '';
	CREATE TABLE images (
		id         INTEGER PRIMARY KEY,
		name       TEXT NOT NULL UNIQUE,
		created_at INTEGER NOT NULL
	);
	CREATE TABLE image_versions (
		id         INTEGER PRIMARY KEY,
		image_id   INTEGER NOT NULL REFERENCES images(id) ON DELETE CASCADE,
		number     INTEGER NOT NULL,
		file       TEXT NOT NULL,
		size       INTEGER NOT NULL,
		source_mac TEXT NOT NULL,
		created_at INTEGER NOT NULL,
		UNIQUE(image_id, number)
	);
`, `
	ALTER TABLE hosts ADD COLUMN image_id INTEGER REFERENCES images(id) ON DELETE SET NULL;
`, `
	ALTER TABLE images ADD COLUMN path TEXT NOT NULL DEFAULT '';
	ALTER TABLE hosts ADD COLUMN saved_table BLOB;
`, `
	ALTER TABLE image_versions ADD COLUMN parent INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE image_versions ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0;
	UPDATE image_versions SET parent = number - 1;
`}

// Setting keys.
const (
	// SettingImageDir is the directory disk images are stored in.
	SettingImageDir = "image_dir"
	// SettingExitMode is how a PC leaves iPXE, see proto.ExitSanboot.
	SettingExitMode = "exit_mode"
)

// Store is an open database.
type Store struct {
	db *sql.DB
}

// Task is a job for one host.
type Task struct {
	ID         int64      `json:"id"`
	Kind       string     `json:"kind"`
	Image      string     `json:"image"`
	Status     string     `json:"status"`
	Percent    int        `json:"percent"`
	Stage      string     `json:"stage"`
	Message    string     `json:"message"`
	CreatedAt  time.Time  `json:"createdAt"`
	FinishedAt *time.Time `json:"finishedAt"`
}

// Host is a registered PC. Task is its active task, if any; LastTask is the
// latest one the agent finished.
type Host struct {
	ID       int64      `json:"id"`
	MAC      string     `json:"mac"`
	Name     string     `json:"name"`
	Image    string     `json:"image"`  // the image this PC belongs to
	Shrunk   bool       `json:"shrunk"` // its volume may still wait to get its size back
	LastSeen *time.Time `json:"lastSeen"`
	Task     *Task      `json:"task"`
	LastTask *Task      `json:"lastTask"`
}

// Discovered is a PC that asked to boot but is not registered.
type Discovered struct {
	MAC       string    `json:"mac"`
	FirstSeen time.Time `json:"firstSeen"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Open opens the database, creating and upgrading it as needed.
func Open(path string) (*Store, error) {
	dsn := path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// The load is a few queries a minute; one connection rules out lock errors.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := s.fillImagePaths(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// migrate brings the schema to the current version.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database version %d is newer than this server (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func unix(t time.Time) int64 { return t.Unix() }

func fromUnix(v int64) time.Time { return time.Unix(v, 0).UTC() }

// Setting returns a stored value, or "" if it was never set.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// SetSetting stores a value.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO settings(key, value) VALUES(?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// AddHost registers a PC and removes it from the discovered list.
func (s *Store) AddHost(ctx context.Context, mac net.HardwareAddr, name string) (Host, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Host{}, err
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hosts WHERE mac = ?)", mac.String()).Scan(&exists); err != nil {
		return Host{}, err
	}
	if exists {
		return Host{}, ErrExists
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO hosts(mac, name, created_at, last_seen)
		VALUES(?1, ?2, ?3, (SELECT last_seen FROM discovered WHERE mac = ?1))`, mac.String(), name, unix(time.Now()))
	if err != nil {
		return Host{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Host{}, err
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM discovered WHERE mac = ?", mac.String()); err != nil {
		return Host{}, err
	}
	if err := tx.Commit(); err != nil {
		return Host{}, err
	}
	return Host{ID: id, MAC: mac.String(), Name: name}, nil
}

// RenameHost changes the name of a host.
func (s *Store) RenameHost(ctx context.Context, id int64, name string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE hosts SET name = ? WHERE id = ?", name, id)
	return affected(res, err)
}

// DeleteHost removes a host and its tasks.
func (s *Store) DeleteHost(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM hosts WHERE id = ?", id)
	return affected(res, err)
}

// affected turns "no row matched" into ErrNotFound.
func affected(res sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// IsHost reports whether a MAC is registered.
func (s *Store) IsHost(ctx context.Context, mac net.HardwareAddr) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM hosts WHERE mac = ?)", mac.String()).Scan(&exists)
	return exists, err
}

// Seen records that a PC asked to boot from the network.
func (s *Store) Seen(ctx context.Context, mac net.HardwareAddr) error {
	now := unix(time.Now())
	res, err := s.db.ExecContext(ctx, "UPDATE hosts SET last_seen = ? WHERE mac = ?", now, mac.String())
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n > 0 {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO discovered(mac, first_seen, last_seen) VALUES(?, ?, ?)
		ON CONFLICT(mac) DO UPDATE SET last_seen = excluded.last_seen`, mac.String(), now, now)
	return err
}

// ForgetDiscovered removes a PC from the discovered list.
func (s *Store) ForgetDiscovered(ctx context.Context, mac net.HardwareAddr) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM discovered WHERE mac = ?", mac.String())
	return affected(res, err)
}

// Hosts lists registered PCs with their active tasks, by name.
func (s *Store) Hosts(ctx context.Context) ([]Host, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT h.id, h.mac, h.name, COALESCE(i.name, ''), h.saved_table IS NOT NULL, h.last_seen, t.id, t.kind, t.image, t.status, t.created_at, t.percent, t.stage, t.message
		FROM hosts h
		LEFT JOIN images i ON i.id = h.image_id
		LEFT JOIN tasks t ON t.host_id = h.id AND t.finished_at IS NULL
		ORDER BY h.name COLLATE NOCASE, h.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	hosts := []Host{}
	for rows.Next() {
		var h Host
		var seen, taskID, created, percent sql.NullInt64
		var kind, image, status, stage, message sql.NullString
		if err := rows.Scan(&h.ID, &h.MAC, &h.Name, &h.Image, &h.Shrunk, &seen, &taskID, &kind, &image, &status, &created, &percent, &stage, &message); err != nil {
			return nil, err
		}
		if seen.Valid {
			t := fromUnix(seen.Int64)
			h.LastSeen = &t
		}
		if taskID.Valid {
			h.Task = &Task{
				ID: taskID.Int64, Kind: kind.String, Image: image.String, Status: status.String, CreatedAt: fromUnix(created.Int64),
				Percent: int(percent.Int64), Stage: stage.String, Message: message.String,
			}
		}
		hosts = append(hosts, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The pool has one connection, so the rows must be closed before the next query.
	rows.Close()

	last, err := s.lastTasks(ctx)
	if err != nil {
		return nil, err
	}
	for i := range hosts {
		hosts[i].LastTask = last[hosts[i].ID]
	}
	return hosts, nil
}

// HostTask is a task with the PC it belongs to.
type HostTask struct {
	Task
	HostID int64  `json:"hostId"`
	Host   string `json:"host"`
}

// RecentTasks lists unfinished tasks and then the latest finished ones.
func (s *Store) RecentTasks(ctx context.Context, limit int) ([]HostTask, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.kind, t.image, t.status, t.created_at, t.finished_at, t.percent, t.stage, t.message, h.id, h.name
		FROM tasks t JOIN hosts h ON h.id = t.host_id
		ORDER BY t.finished_at IS NOT NULL, t.id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	tasks := []HostTask{}
	for rows.Next() {
		var t HostTask
		var created int64
		var finished sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Kind, &t.Image, &t.Status, &created, &finished, &t.Percent, &t.Stage, &t.Message, &t.HostID, &t.Host); err != nil {
			return nil, err
		}
		t.CreatedAt = fromUnix(created)
		if finished.Valid {
			end := fromUnix(finished.Int64)
			t.FinishedAt = &end
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

// lastTasks returns the latest task the agent finished on each host.
func (s *Store) lastTasks(ctx context.Context) (map[int64]*Task, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT host_id, id, kind, status, created_at, finished_at, percent, stage, message
		FROM tasks
		WHERE id IN (SELECT MAX(id) FROM tasks WHERE status IN (?, ?) GROUP BY host_id)`, StatusDone, StatusFailed)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	last := map[int64]*Task{}
	for rows.Next() {
		var hostID, created, finished int64
		var t Task
		if err := rows.Scan(&hostID, &t.ID, &t.Kind, &t.Status, &created, &finished, &t.Percent, &t.Stage, &t.Message); err != nil {
			return nil, err
		}
		end := fromUnix(finished)
		t.CreatedAt, t.FinishedAt = fromUnix(created), &end
		last[hostID] = &t
	}
	return last, rows.Err()
}

// DiscoveredPCs lists unregistered PCs, most recent first.
func (s *Store) DiscoveredPCs(ctx context.Context) ([]Discovered, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT mac, first_seen, last_seen FROM discovered ORDER BY last_seen DESC, mac")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := []Discovered{}
	for rows.Next() {
		var d Discovered
		var first, last int64
		if err := rows.Scan(&d.MAC, &first, &last); err != nil {
			return nil, err
		}
		d.FirstSeen, d.LastSeen = fromUnix(first), fromUnix(last)
		list = append(list, d)
	}
	return list, rows.Err()
}

// CreateTask queues a task for a host. image is the target of a clone, otherwise empty.
func (s *Store) CreateTask(ctx context.Context, hostID int64, kind, image string) (Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Task{}, err
	}
	defer tx.Rollback()

	var hostExists, taskExists bool
	err = tx.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM hosts WHERE id = ?1),
		EXISTS(SELECT 1 FROM tasks WHERE host_id = ?1 AND finished_at IS NULL)`, hostID).Scan(&hostExists, &taskExists)
	if err != nil {
		return Task{}, err
	}
	if !hostExists {
		return Task{}, ErrNotFound
	}
	if taskExists {
		return Task{}, ErrTaskExists
	}
	now := time.Now()
	res, err := tx.ExecContext(ctx, "INSERT INTO tasks(host_id, kind, image, status, created_at) VALUES(?, ?, ?, ?, ?)", hostID, kind, image, StatusQueued, unix(now))
	if err != nil {
		return Task{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Task{}, err
	}
	if err := tx.Commit(); err != nil {
		return Task{}, err
	}
	return Task{ID: id, Kind: kind, Image: image, Status: StatusQueued, CreatedAt: fromUnix(unix(now))}, nil
}

// CancelTask ends the active task of a host, so it boots from disk again.
func (s *Store) CancelTask(ctx context.Context, hostID int64) error {
	res, err := s.db.ExecContext(ctx, "UPDATE tasks SET status = ?, finished_at = ? WHERE host_id = ? AND finished_at IS NULL",
		StatusCancelled, unix(time.Now()), hostID)
	return affected(res, err)
}

// HasTask reports whether a MAC has an active task.
func (s *Store) HasTask(ctx context.Context, mac net.HardwareAddr) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM tasks t JOIN hosts h ON h.id = t.host_id
		WHERE h.mac = ? AND t.finished_at IS NULL)`, mac.String()).Scan(&exists)
	return exists, err
}

// HostName returns the name of a registered PC.
func (s *Store) HostName(ctx context.Context, mac net.HardwareAddr) (string, error) {
	var name string
	err := s.db.QueryRowContext(ctx, "SELECT name FROM hosts WHERE mac = ?", mac.String()).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return name, err
}

// TaskOpen reports whether a task exists and has not finished, for any PC.
func (s *Store) TaskOpen(ctx context.Context, id int64) (bool, error) {
	var open bool
	err := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM tasks WHERE id = ? AND finished_at IS NULL)", id).Scan(&open)
	return open, err
}

// SavedTable returns the partition table kept for a PC, or nil.
func (s *Store) SavedTable(ctx context.Context, mac net.HardwareAddr) ([]byte, error) {
	var table []byte
	err := s.db.QueryRowContext(ctx, "SELECT saved_table FROM hosts WHERE mac = ?", mac.String()).Scan(&table)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return table, err
}

// SetSavedTable keeps the partition table of a PC; an empty one removes it.
func (s *Store) SetSavedTable(ctx context.Context, mac net.HardwareAddr, table []byte) error {
	var value any
	if len(table) > 0 {
		value = table
	}
	res, err := s.db.ExecContext(ctx, "UPDATE hosts SET saved_table = ? WHERE mac = ?", value, mac.String())
	return affected(res, err)
}

// ActiveTask returns the active task of a PC, or nil if it has none.
func (s *Store) ActiveTask(ctx context.Context, mac net.HardwareAddr) (*Task, error) {
	var t Task
	var created int64
	err := s.db.QueryRowContext(ctx, `
		SELECT t.id, t.kind, t.image, t.status, t.created_at
		FROM tasks t JOIN hosts h ON h.id = t.host_id
		WHERE h.mac = ? AND t.finished_at IS NULL`, mac.String()).Scan(&t.ID, &t.Kind, &t.Image, &t.Status, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = fromUnix(created)
	return &t, nil
}

// ReportProgress updates a running task. It returns ErrNotFound if the task is
// not an active task of this PC, for example after the operator cancelled it.
func (s *Store) ReportProgress(ctx context.Context, mac net.HardwareAddr, id int64, percent int, stage, message string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, percent = ?, stage = ?, message = ?
		WHERE id = ? AND finished_at IS NULL AND host_id = (SELECT id FROM hosts WHERE mac = ?)`,
		StatusRunning, percent, stage, message, id, mac.String())
	return affected(res, err)
}

// FinishTask ends a task with the agent's result, so the PC boots from disk again.
func (s *Store) FinishTask(ctx context.Context, mac net.HardwareAddr, id int64, ok bool, message string) error {
	status, percent := StatusFailed, "percent"
	if ok {
		status, percent = StatusDone, "100"
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, percent = `+percent+`, message = ?, finished_at = ?
		WHERE id = ? AND finished_at IS NULL AND host_id = (SELECT id FROM hosts WHERE mac = ?)`,
		status, message, unix(time.Now()), id, mac.String())
	return affected(res, err)
}

// MarkStarted records that the PC was sent into WinPE for its task. After a
// reboot in the middle of a task the progress starts over.
func (s *Store) MarkStarted(ctx context.Context, mac net.HardwareAddr) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE tasks SET status = ?, percent = 0, stage = '', message = '', started_at = COALESCE(started_at, ?)
		WHERE finished_at IS NULL AND host_id = (SELECT id FROM hosts WHERE mac = ?)`,
		StatusStarted, unix(time.Now()), mac.String())
	return err
}
