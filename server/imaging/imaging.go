// Package imaging receives clones: it turns the blocks an agent sends into the
// next file of an image chain.
//
// A file being received lives in .incoming and moves into the image directory
// only after it is complete and readable, so an interrupted clone can never
// replace or look like a working image.
package imaging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"fogs/proto"
	"fogs/server/store"
)

const incomingDir = ".incoming"

// idleLimit is how long a clone may stay silent. An agent sends a batch every
// few seconds, so this only catches one that is gone.
const idleLimit = 15 * time.Minute

// Errors callers can tell apart.
var (
	// ErrNoImageDir means the operator has not chosen a directory yet.
	ErrNoImageDir = errors.New("imaging: image directory is not set")
	// ErrBusy means another clone of the same image is running.
	ErrBusy = errors.New("imaging: image is being cloned")
	// ErrInUse means a PC is still reading or writing the image, for example after
	// its task was cancelled: the server lets go of it within a minute.
	ErrInUse = errors.New("imaging: image is in use by a PC")
	// ErrNoSession means there is no clone in progress for the task.
	ErrNoSession = errors.New("imaging: no clone in progress")
)

// Store is the part of the database the manager uses.
type Store interface {
	ImagePath(context.Context, string) (string, error)
	LatestVersion(context.Context, string) (*store.Version, error)
	AddVersion(context.Context, string, store.Version) error
	Versions(context.Context, string) ([]store.Version, error)
	AllVersions(context.Context, string) ([]store.Version, error)
	NextNumber(context.Context, string) (int, error)
	TaskOpen(ctx context.Context, id int64) (bool, error)
	DisableAfter(context.Context, string, int) error
	EnableVersions(context.Context, string, []int) error
	ReplaceVersions(context.Context, string, store.Version) error
	Setting(context.Context, string) (string, error)
	Images(context.Context) ([]store.Image, error)
}

// Manager runs the clones in progress, one per task.
type Manager struct {
	store Store
	log   *slog.Logger

	mu       sync.Mutex
	sessions map[int64]*session
	deploys  map[int64]*deploySession
	claims   map[string]int64 // the task whose turn it is to clone an image

	merging     map[string]bool   // images being merged
	mergeErrors map[string]string // how the last merge of an image failed

	statsMu sync.Mutex // apart from mu: a Put holds only its session, and notes as it goes
	stats   map[int64]*taskStats
}

// New returns a manager.
func New(s Store, log *slog.Logger) *Manager {
	return &Manager{store: s, log: log, sessions: map[int64]*session{}, deploys: map[int64]*deploySession{}, claims: map[string]int64{},
		merging: map[string]bool{}, mergeErrors: map[string]string{}, stats: map[int64]*taskStats{}}
}

// session is one file being received. Its lock orders the writes of one clone.
type session struct {
	mu     sync.Mutex
	image  string
	mac    string
	dir    string // directory of the image
	name   string // final file name
	number int
	parent int    // the version this one builds on, 0 for an .ldisk
	path   string // file in .incoming
	file   *os.File
	buf    *bufio.Writer
	writer *proto.Writer

	active atomic.Int64 // unix time of the last request
}

func (s *session) touch() { s.active.Store(time.Now().Unix()) }

func (s *session) seen() time.Time { return time.Unix(s.active.Load(), 0) }

// discard releases the file and removes what was received.
func (s *session) discard() {
	s.file.Close()
	os.Remove(s.path)
}

// Claim gives a task the turn to clone an image, or ErrBusy while another task has
// it. One clone per image runs at a time, and a clone asks before it shrinks the
// volume, long before Start. Only a task that reached its agent holds a turn, so a
// PC that is switched off does not block anyone; a task that has ended frees it.
func (m *Manager) Claim(ctx context.Context, task int64, image string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claim(ctx, task, image)
}

func (m *Manager) claim(ctx context.Context, task int64, image string) error {
	if m.merging[image] {
		return ErrBusy
	}
	if owner, ok := m.claims[image]; ok && owner != task {
		open, err := m.store.TaskOpen(ctx, owner)
		if err != nil {
			return err
		}
		if open {
			return ErrBusy
		}
	}
	for id, s := range m.sessions {
		if s.image != image || id == task {
			continue
		}
		open, err := m.store.TaskOpen(ctx, id)
		if err != nil {
			return err
		}
		if open {
			return ErrBusy
		}
		s.discard() // its task was cancelled and its agent never said so
		delete(m.sessions, id)
	}
	m.claims[image] = task
	return nil
}

// Start begins a clone. A clone already running for the task is dropped: the
// agent started over, for example after the operator pressed retry.
func (m *Manager) Start(ctx context.Context, task int64, image, mac string) (proto.CloneInfo, error) {
	if !proto.ValidImageName(image) {
		return proto.CloneInfo{}, fmt.Errorf("imaging: bad image name %q", image)
	}
	dir, err := m.store.ImagePath(ctx, image)
	if err != nil {
		return proto.CloneInfo{}, err
	}
	if dir == "" {
		return proto.CloneInfo{}, ErrNoImageDir
	}

	// The index of the parent is read before the lock, which every request of every
	// clone and deploy needs: a slow disk must hold up only this clone. Under the
	// lock the parent is still the newest version, or the clone starts over.
	var latest *store.Version
	var parent *proto.File
	for {
		if latest, parent, err = m.readParent(ctx, dir, image); err != nil {
			return proto.CloneInfo{}, err
		}
		m.mu.Lock()
		now, err := m.store.LatestVersion(ctx, image)
		if err != nil {
			m.mu.Unlock()
			return proto.CloneInfo{}, err
		}
		if sameVersion(now, latest) {
			break
		}
		m.mu.Unlock()
	}
	defer m.mu.Unlock()
	if old := m.sessions[task]; old != nil {
		old.discard()
		delete(m.sessions, task)
	}
	if err := m.claim(ctx, task, image); err != nil {
		return proto.CloneInfo{}, err
	}

	header := proto.Header{Kind: proto.KindDisk, BlockSize: proto.DefaultBlockSize, ID: proto.NewID()}
	var parentIndex *proto.Index
	parentNumber, ext := 0, proto.ExtDisk
	number, err := m.store.NextNumber(ctx, image)
	if err != nil {
		return proto.CloneInfo{}, err
	}
	if latest != nil {
		header.Kind, header.BlockSize = proto.KindDelta, parent.Header.BlockSize
		header.Parent, header.ParentSum = parent.Header.ID, parent.Sum
		parentIndex = parent.Index
		parentNumber, ext = latest.Number, proto.ExtDelta
	}

	// Next to the image, on the same disk: a finished file moves by renaming.
	incoming := filepath.Join(filepath.Dir(dir), incomingDir)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		return proto.CloneInfo{}, err
	}
	path := filepath.Join(incoming, strconv.FormatInt(task, 10)+".part")
	file, err := os.Create(path)
	if err != nil {
		return proto.CloneInfo{}, err
	}
	buf := bufio.NewWriterSize(file, 1<<20)
	writer, err := proto.NewWriter(buf, header, parentIndex)
	if err != nil {
		file.Close()
		os.Remove(path)
		return proto.CloneInfo{}, err
	}
	s := &session{
		image: image, mac: mac, dir: dir, number: number, parent: parentNumber,
		name: "v" + strconv.Itoa(number) + ext,
		path: path, file: file, buf: buf, writer: writer,
	}
	s.touch()
	m.sessions[task] = s
	m.log.Info("clone started", "task", task, "image", image, "version", number)
	return proto.CloneInfo{BlockSize: header.BlockSize, Image: image, Version: number}, nil
}

// readParent returns the newest version of an image and its opened file, or nil
// for an image without versions.
func (m *Manager) readParent(ctx context.Context, dir, image string) (*store.Version, *proto.File, error) {
	latest, err := m.store.LatestVersion(ctx, image)
	if err != nil || latest == nil {
		return nil, nil, err
	}
	parent, err := openFile(filepath.Join(dir, latest.File))
	if err != nil {
		return nil, nil, fmt.Errorf("imaging: parent %s: %w", latest.File, err)
	}
	return latest, parent, nil
}

func sameVersion(a, b *store.Version) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Number == b.Number && a.File == b.File
}

// CleanIncoming removes what clones and merges left unfinished when the server
// stopped. It runs before any agent can start a clone. Every image keeps its
// .incoming next to its own folder, so each of those places is looked at.
func (m *Manager) CleanIncoming(ctx context.Context) error {
	places := map[string]bool{}
	if dir, err := m.store.Setting(ctx, store.SettingImageDir); err != nil {
		return err
	} else if dir != "" {
		places[dir] = true
	}
	images, err := m.store.Images(ctx)
	if err != nil {
		return err
	}
	for _, image := range images {
		if image.Path != "" {
			places[filepath.Dir(image.Path)] = true
		}
	}
	for place := range places {
		parts, _ := filepath.Glob(filepath.Join(place, incomingDir, "*.part"))
		for _, part := range parts {
			if err := os.Remove(part); err != nil {
				m.log.Warn("unfinished file not removed", "file", part, "err", err)
				continue
			}
			m.log.Info("unfinished file removed", "file", part)
		}
	}
	return nil
}

// openFile opens an image file and keeps it only long enough to read its index.
func openFile(path string) (*proto.File, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return proto.OpenFile(f, info.Size())
}

func (m *Manager) session(task int64) (*session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[task]
	if s == nil {
		return nil, ErrNoSession
	}
	s.touch()
	return s, nil
}

// Check answers which hashes the server lacks: byte i is 1 if block i must be sent.
func (m *Manager) Check(task int64, hashes []proto.Sum) ([]byte, error) {
	s, err := m.session(task)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	started := time.Now()
	defer func() { m.note(task, statCheck, time.Since(started)) }()
	missing := make([]byte, len(hashes))
	for i, h := range hashes {
		if !s.writer.Has(h) {
			missing[i] = 1
		}
	}
	return missing, nil
}

// Put stores a stream of records.
func (m *Manager) Put(task int64, r io.Reader) error {
	s, err := m.session(task)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Timed per record and noted once, so a request costs two lock rounds and not
	// two per block.
	var receiving, writing time.Duration
	var stored uint64
	defer func() {
		m.note(task, statReceive, receiving)
		m.note(task, statWriteImage, writing)
		m.count(task, "записано в файл образа", stored, true)
	}()
	for {
		at := time.Now()
		rec, err := proto.ReadPut(r)
		receiving += time.Since(at)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		at = time.Now()
		if rec.Block != nil {
			if rec.Block.Hash != rec.Hash {
				return fmt.Errorf("%w: record names another block", proto.ErrCorrupt)
			}
			if err := s.writer.WriteBlock(*rec.Block, rec.Payload); err != nil {
				return err
			}
			stored += uint64(proto.BlockHeaderSize + len(rec.Payload))
		}
		err = s.writer.Put(rec.Partition, rec.Position, rec.Hash)
		writing += time.Since(at)
		if err != nil {
			return err
		}
	}
}

// Finish completes the file and publishes it as the next version. On failure
// nothing is published and the clone has to start over.
func (m *Manager) Finish(ctx context.Context, task int64, manifest proto.Manifest) (proto.CloneResult, error) {
	m.mu.Lock()
	s := m.sessions[task]
	delete(m.sessions, task)
	m.mu.Unlock()
	if s == nil {
		return proto.CloneResult{}, ErrNoSession
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	started := time.Now()
	defer func() { m.note(task, statFinish, time.Since(started)) }()

	size, err := s.complete(manifest)
	if err != nil {
		s.discard()
		return proto.CloneResult{}, err
	}
	final := filepath.Join(s.dir, s.name)
	if err := publish(s.path, final, s.dir); err != nil {
		os.Remove(s.path)
		return proto.CloneResult{}, err
	}
	version := store.Version{Number: s.number, File: s.name, Size: size, SourceMAC: s.mac, Parent: s.parent}
	if err := m.store.AddVersion(ctx, s.image, version); err != nil {
		os.Remove(final)
		return proto.CloneResult{}, err
	}
	m.log.Info("clone finished", "task", task, "image", s.image, "file", s.name, "bytes", size)
	return proto.CloneResult{File: s.name, Size: size}, nil
}

// complete writes the index and proves the result opens before anyone relies on it.
func (s *session) complete(manifest proto.Manifest) (int64, error) {
	if _, err := s.writer.Close(manifest); err != nil {
		return 0, err
	}
	if err := s.buf.Flush(); err != nil {
		return 0, err
	}
	if err := s.file.Sync(); err != nil {
		return 0, err
	}
	info, err := s.file.Stat()
	if err != nil {
		return 0, err
	}
	if _, err := proto.OpenFile(s.file, info.Size()); err != nil {
		return 0, err
	}
	return info.Size(), s.file.Close()
}

// publish moves a finished file into place without overwriting anything.
func publish(from, to, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(to); err == nil {
		return fmt.Errorf("imaging: %s already exists", to)
	}
	return os.Rename(from, to)
}

// Run drops clones whose agent went silent until ctx ends, then all of them. A
// PC switched off in the middle of a clone never says goodbye.
func (m *Manager) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			m.sweep(time.Now().Add(idleLimit))
			return
		case now := <-tick.C:
			m.dropEnded(ctx)
			m.sweep(now)
		}
	}
}

// sweep drops every clone that has been idle for idleLimit at the given moment.
func (m *Manager) sweep(now time.Time) {
	m.mu.Lock()
	var stale []int64
	for task, s := range m.deploys {
		if now.Sub(time.Unix(s.active.Load(), 0)) >= idleLimit {
			stale = append(stale, task)
		}
	}
	for task, s := range m.sessions {
		if now.Sub(s.seen()) >= idleLimit {
			stale = append(stale, task)
		}
	}
	m.mu.Unlock()
	for _, task := range stale {
		m.Abort(task)
	}
}

// dropEnded drops the clones and deploys whose task has ended, for example because
// the operator cancelled it: the agent may never ask again, and its deploy would
// hold the files of the image until idleLimit, keeping a merge from removing them.
func (m *Manager) dropEnded(ctx context.Context) {
	m.mu.Lock()
	var tasks []int64
	for task := range m.sessions {
		tasks = append(tasks, task)
	}
	for task := range m.deploys {
		tasks = append(tasks, task)
	}
	m.mu.Unlock()
	// The stats of a task stay until it has ended: its result, and the failure
	// reports before it, carry them.
	m.statsMu.Lock()
	for task := range m.stats {
		tasks = append(tasks, task)
	}
	m.statsMu.Unlock()
	slices.Sort(tasks)
	for _, task := range slices.Compact(tasks) {
		open, err := m.store.TaskOpen(ctx, task)
		if err != nil {
			m.log.Warn("task state unknown", "task", task, "err", err)
			continue
		}
		if !open {
			m.Abort(task)
			m.forgetStats(task)
		}
	}
}

// Abort drops a clone, for example when the operator cancelled the task.
func (m *Manager) Abort(task int64) {
	m.mu.Lock()
	s := m.sessions[task]
	delete(m.sessions, task)
	d := m.deploys[task]
	delete(m.deploys, task)
	m.mu.Unlock()
	if d != nil {
		d.close()
	}
	if s != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.discard()
		m.log.Info("clone dropped", "task", task, "image", s.image)
	}
}
