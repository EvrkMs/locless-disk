package imaging

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"fogs/proto"
)

// ErrNoImage means the image has no versions to deploy.
var ErrNoImage = errors.New("imaging: image has no versions")

// deploySession is one image being sent to one PC. The files stay open for the
// whole deploy, so merging or deleting versions later cannot pull them away.
type deploySession struct {
	image   string
	files   []*os.File
	chain   *proto.Chain
	entries []proto.Entry
	active  atomic.Int64
}

func (s *deploySession) close() {
	for _, f := range s.files {
		f.Close()
	}
}

// StartDeploy opens the newest finished version of an image for a task. A clone in
// progress does not matter: its file is not a version until it is complete. Any
// number of deploys may run; only a merge, which replaces the files, makes them wait.
func (m *Manager) StartDeploy(ctx context.Context, task int64, image string) (proto.DeployInfo, error) {
	if !proto.ValidImageName(image) {
		return proto.DeployInfo{}, fmt.Errorf("imaging: bad image name %q", image)
	}
	dir, err := m.store.ImagePath(ctx, image)
	if err != nil {
		return proto.DeployInfo{}, err
	}
	if dir == "" {
		return proto.DeployInfo{}, ErrNoImageDir
	}
	versions, err := m.store.Versions(ctx, image)
	if err != nil {
		return proto.DeployInfo{}, err
	}
	if len(versions) == 0 {
		return proto.DeployInfo{}, ErrNoImage
	}
	// Asked before the files are opened as well, or the files of a merge that is
	// just ending could vanish under openChain.
	if merging, _ := m.Merging(image); merging {
		return proto.DeployInfo{}, ErrBusy
	}

	opened := time.Now()
	s, err := openChain(dir, versions)
	m.note(task, statOpen, time.Since(opened))
	if err != nil {
		return proto.DeployInfo{}, err
	}
	s.image = image
	index := s.chain.Index()
	s.entries = index.Entries
	s.active.Store(time.Now().Unix())

	m.mu.Lock()
	if m.merging[image] {
		m.mu.Unlock()
		s.close()
		return proto.DeployInfo{}, ErrBusy
	}
	if old := m.deploys[task]; old != nil {
		old.close()
	}
	m.deploys[task] = s
	m.mu.Unlock()

	last := versions[len(versions)-1]
	m.log.Info("deploy started", "task", task, "image", image, "version", last.Number, "blocks", len(s.entries))
	return proto.DeployInfo{BlockSize: s.chain.BlockSize(), Image: image, Version: last.Number, Manifest: index.Manifest}, nil
}

func (m *Manager) deploy(task int64) (*deploySession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.deploys[task]
	if s == nil {
		return nil, ErrNoSession
	}
	s.active.Store(time.Now().Unix())
	return s, nil
}

// DeployList writes the list of places the agent has to fill.
func (m *Manager) DeployList(task int64, w io.Writer) error {
	s, err := m.deploy(task)
	if err != nil {
		return err
	}
	started := time.Now()
	defer func() { m.note(task, statList, time.Since(started)) }()
	b := make([]byte, 0, 40*len(s.entries))
	for _, e := range s.entries {
		b = proto.AppendDeployEntry(b, proto.DeployEntry{Partition: e.Partition, Position: e.Position, Hash: e.Hash})
	}
	_, err = w.Write(b)
	return err
}

// DeployBlocks writes the stored blocks with the given numbers of the list, as
// they are in the file: the agent checks them, so a bad block is caught there.
func (m *Manager) DeployBlocks(task int64, numbers []uint32, w io.Writer) error {
	s, err := m.deploy(task)
	if err != nil {
		return err
	}
	// Timed per block and noted once, like Put.
	var reading, sending time.Duration
	var sent uint64
	defer func() {
		m.note(task, statReadImage, reading)
		m.note(task, statSend, sending)
		m.count(task, "отдано по сети", sent, true)
	}()
	for _, n := range numbers {
		if int64(n) >= int64(len(s.entries)) {
			return fmt.Errorf("imaging: no entry %d", n)
		}
		at := time.Now()
		h, payload, err := s.chain.Stored(s.entries[n])
		reading += time.Since(at)
		if err != nil {
			return err
		}
		at = time.Now()
		err = proto.WriteStored(w, h, payload)
		sending += time.Since(at)
		if err != nil {
			return err
		}
		sent += uint64(proto.BlockHeaderSize + len(payload))
	}
	return nil
}
