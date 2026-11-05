package imaging

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"fogs/proto"
	"fogs/recovery"
	"fogs/server/store"
)

// Errors the operator can be told about.
var (
	// ErrForked means a newer version was cloned on top of the parent, so the
	// disabled version no longer continues the chain.
	ErrForked = errors.New("imaging: the chain went another way")
	// ErrBrokenChain means the versions the database names do not fit together.
	ErrBrokenChain = errors.New("imaging: broken chain")
	// ErrNothingToMerge means the image is a single file already.
	ErrNothingToMerge = errors.New("imaging: nothing to merge")
)

// openChain opens versions, oldest first, and proves from the files themselves
// that they follow each other. The caller closes the result.
func openChain(dir string, versions []store.Version) (*deploySession, error) {
	s := &deploySession{}
	var files []*proto.File
	for _, v := range versions {
		f, err := os.Open(filepath.Join(dir, v.File))
		if err != nil {
			s.close()
			return nil, err
		}
		s.files = append(s.files, f)
		info, err := f.Stat()
		if err != nil {
			s.close()
			return nil, err
		}
		pf, err := proto.OpenFile(f, info.Size())
		if err != nil {
			s.close()
			return nil, fmt.Errorf("imaging: %s: %w", v.File, err)
		}
		files = append(files, pf)
	}
	chain, err := proto.NewChain(files...)
	if err != nil {
		s.close()
		return nil, err
	}
	s.chain = chain
	return s, nil
}

// checkChain opens versions only to see that they form a chain.
func (m *Manager) checkChain(ctx context.Context, image string, versions []store.Version) error {
	dir, err := m.store.ImagePath(ctx, image)
	if err != nil {
		return err
	}
	s, err := openChain(dir, versions)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBrokenChain, err)
	}
	s.close()
	return nil
}

// Restore makes an older version the current one. The versions after it are only
// switched off: their files stay, and Enable can bring them back.
func (m *Manager) Restore(ctx context.Context, image string, number int) error {
	versions, err := m.store.Versions(ctx, image)
	if err != nil {
		return err
	}
	for i, v := range versions {
		if v.Number == number {
			if err := m.checkChain(ctx, image, versions[:i+1]); err != nil {
				return err
			}
			m.log.Info("image restored", "image", image, "version", number)
			return m.store.DisableAfter(ctx, image, number)
		}
	}
	return store.ErrNotFound
}

// Enable brings a switched off version back, with the switched off versions it
// stands on. It works while the version still continues the chain: once a clone
// has gone on from an earlier version, the two are different histories.
func (m *Manager) Enable(ctx context.Context, image string, number int) error {
	all, err := m.store.AllVersions(ctx, image)
	if err != nil {
		return err
	}
	byNumber := map[int]store.Version{}
	for _, v := range all {
		byNumber[v.Number] = v
	}
	v, ok := byNumber[number]
	if !ok || !v.Disabled {
		return store.ErrNotFound
	}
	var path []store.Version // from the oldest switched off version to this one
	for v.Disabled {
		path = append([]store.Version{v}, path...)
		parent, ok := byNumber[v.Parent]
		if !ok {
			return fmt.Errorf("%w: %s has no parent", ErrBrokenChain, v.File)
		}
		v = parent
	}
	active, err := m.store.Versions(ctx, image)
	if err != nil {
		return err
	}
	if len(active) == 0 || active[len(active)-1].Number != v.Number {
		return ErrForked
	}
	if err := m.checkChain(ctx, image, append(active, path...)); err != nil {
		return err
	}
	numbers := make([]int, len(path))
	for i, p := range path {
		numbers[i] = p.Number
	}
	m.log.Info("version enabled", "image", image, "versions", numbers)
	return m.store.EnableVersions(ctx, image, numbers)
}

// Merging says whether a merge of the image runs now, and how the last one failed.
func (m *Manager) Merging(image string) (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.merging[image], m.mergeErrors[image]
}

// StartMerge folds the current chain of an image into one new .ldisk in the
// background. Everything else of the image, switched off versions included, is
// removed once the new file is in place.
func (m *Manager) StartMerge(ctx context.Context, image string) error {
	versions, err := m.store.Versions(ctx, image)
	if err != nil {
		return err
	}
	if len(versions) < 2 {
		return ErrNothingToMerge
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.merging[image] {
		return ErrBusy
	}
	for _, s := range m.sessions {
		if s.image == image {
			return ErrBusy
		}
	}
	m.merging[image] = true
	delete(m.mergeErrors, image)
	go func() {
		// The request that started this is long over.
		err := recovery.Run(func() error { return m.merge(context.Background(), image) })
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.merging, image)
		if err != nil {
			m.mergeErrors[image] = err.Error()
			m.log.Error("merge failed", "image", image, "err", err, "stack", recovery.StackOf(err))
		}
	}()
	return nil
}

// merge copies every block the newest version needs, still compressed, and swaps
// the files. Until the database says otherwise the old chain stays in charge, so a
// failure at any point leaves the image as it was.
func (m *Manager) merge(ctx context.Context, image string) error {
	dir, err := m.store.ImagePath(ctx, image)
	if err != nil {
		return err
	}
	versions, err := m.store.Versions(ctx, image)
	if err != nil {
		return err
	}
	all, err := m.store.AllVersions(ctx, image)
	if err != nil {
		return err
	}
	number, err := m.store.NextNumber(ctx, image)
	if err != nil {
		return err
	}
	src, err := openChain(dir, versions)
	if err != nil {
		return err
	}
	defer src.close()

	incoming := filepath.Join(filepath.Dir(dir), incomingDir)
	if err := os.MkdirAll(incoming, 0o755); err != nil {
		return err
	}
	path := filepath.Join(incoming, "merge-"+image+".part")
	size, err := writeMerged(path, src.chain)
	if err != nil {
		os.Remove(path)
		return err
	}
	name := "v" + strconv.Itoa(number) + proto.ExtDisk
	final := filepath.Join(dir, name)
	if err := publish(path, final, dir); err != nil {
		os.Remove(path)
		return err
	}
	last := versions[len(versions)-1]
	merged := store.Version{Number: number, File: name, Size: size, SourceMAC: last.SourceMAC}
	if err := m.store.ReplaceVersions(ctx, image, merged); err != nil {
		os.Remove(final)
		return err
	}
	src.close()
	src.files = nil
	for _, v := range all {
		if err := os.Remove(filepath.Join(dir, v.File)); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("old version not removed", "image", image, "file", v.File, "err", err)
		}
	}
	m.log.Info("image merged", "image", image, "file", name, "bytes", size, "files", len(all))
	return nil
}

// writeMerged writes the disk a chain ends in as one full image and proves it opens.
func writeMerged(path string, chain *proto.Chain) (int64, error) {
	file, err := os.Create(path)
	if err != nil {
		return 0, err
	}
	defer file.Close()
	buf := bufio.NewWriterSize(file, 1<<20)
	header := proto.Header{Kind: proto.KindDisk, BlockSize: chain.BlockSize(), ID: proto.NewID()}
	w, err := proto.NewWriter(buf, header, nil)
	if err != nil {
		return 0, err
	}
	index := chain.Index()
	for _, e := range index.Entries {
		if !e.Zero() && !w.Has(e.Hash) {
			h, payload, err := chain.Stored(e)
			if err != nil {
				return 0, err
			}
			if err := w.WriteBlock(h, payload); err != nil {
				return 0, err
			}
		}
		if err := w.Put(e.Partition, e.Position, e.Hash); err != nil {
			return 0, err
		}
	}
	if _, err := w.Close(index.Manifest); err != nil {
		return 0, err
	}
	if err := buf.Flush(); err != nil {
		return 0, err
	}
	if err := file.Sync(); err != nil {
		return 0, err
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if _, err := proto.OpenFile(file, info.Size()); err != nil {
		return 0, err
	}
	return info.Size(), nil
}
