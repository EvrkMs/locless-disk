package imaging

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fogs/proto"
	"fogs/server/store"
)

const (
	testGUID = "12345678-9abc-def0-1234-56789abcdef0"
	mac      = "04:7c:16:e7:d2:1e"
)

var ctx = context.Background()

func manifest() proto.Manifest {
	return proto.Manifest{
		Version: proto.ManifestVersion, DiskSize: 64 << 20, SectorSize: 512, DiskGUID: testGUID,
		Partitions: []proto.Partition{{
			Number: 3, TypeGUID: testGUID, GUID: testGUID, Offset: 1 << 20, Size: 32 << 20,
			Policy: proto.PolicyFill, Content: proto.ContentBlocks,
		}},
	}
}

func setup(t *testing.T) (*Manager, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := store.Open(filepath.Join(dir, "db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	images := filepath.Join(dir, "images")
	if err := s.SetSetting(ctx, store.SettingImageDir, images); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	return New(s, slog.New(slog.NewTextHandler(io.Discard, nil))), s, images
}

// block returns distinct, poorly compressible data for a seed.
func block(seed byte) []byte {
	b := make([]byte, proto.DefaultBlockSize)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// clone sends blocks the way the agent does: check first, data only if missing.
func clone(t *testing.T, m *Manager, task int64, blocks map[uint32][]byte) (proto.CloneResult, int) {
	t.Helper()
	if _, err := m.Start(ctx, task, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	var positions []uint32
	var hashes []proto.Sum
	for pos, raw := range blocks {
		positions = append(positions, pos)
		hashes = append(hashes, proto.HashBlock(raw))
	}
	missing, err := m.Check(task, hashes)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	var body []byte
	sent := 0
	for i, pos := range positions {
		rec := proto.PutRecord{Partition: 3, Position: pos, Hash: hashes[i]}
		if missing[i] == 1 {
			h, payload, err := proto.EncodeBlock(blocks[pos])
			if err != nil {
				t.Fatalf("EncodeBlock: %v", err)
			}
			rec.Block, rec.Payload = &h, payload
			sent++
		}
		body = proto.AppendPut(body, rec)
	}
	if err := m.Put(task, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	res, err := m.Finish(ctx, task, manifest())
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	return res, sent
}

func testChain(t *testing.T, dir string, names ...string) *proto.Chain {
	t.Helper()
	var files []*proto.File
	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, "win11", name))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { f.Close() })
		info, _ := f.Stat()
		pf, err := proto.OpenFile(f, info.Size())
		if err != nil {
			t.Fatalf("OpenFile: %v", err)
		}
		files = append(files, pf)
	}
	c, err := proto.NewChain(files...)
	if err != nil {
		t.Fatalf("NewChain: %v", err)
	}
	return c
}

func TestFirstCloneThenDelta(t *testing.T) {
	m, s, dir := setup(t)

	first, sent := clone(t, m, 1, map[uint32][]byte{0: block(1), 1: block(2), 2: block(3)})
	if first.File != "v1.ldisk" || sent != 3 {
		t.Fatalf("first: %+v, sent %d", first, sent)
	}

	second, sent := clone(t, m, 2, map[uint32][]byte{0: block(1), 1: block(9), 2: block(3)})
	if second.File != "v2.ldelta" || sent != 1 {
		t.Fatalf("second: %+v, sent %d blocks, want only the changed one", second, sent)
	}
	if second.Size >= first.Size/2 {
		t.Fatalf("delta is %d bytes, image is %d", second.Size, first.Size)
	}

	c := testChain(t, dir, "v1.ldisk", "v2.ldelta")
	want := map[uint32][]byte{0: block(1), 1: block(9), 2: block(3)}
	for _, e := range c.Index().Entries {
		raw, err := c.Read(e)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if !bytes.Equal(raw, want[e.Position]) {
			t.Fatalf("block %d differs", e.Position)
		}
	}

	images, err := s.Images(ctx)
	if err != nil || len(images) != 1 || len(images[0].Versions) != 2 {
		t.Fatalf("images %+v, err %v", images, err)
	}
	if v := images[0].Versions[1]; v.File != "v2.ldelta" || v.Size != second.Size || v.SourceMAC != mac {
		t.Fatalf("version: %+v", v)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, incomingDir)); len(entries) != 0 {
		t.Fatalf("left in .incoming: %v", entries)
	}
}

func TestStartRejects(t *testing.T) {
	m, s, _ := setup(t)
	if _, err := m.Start(ctx, 1, "../evil", mac); err == nil {
		t.Fatal("Start accepted a path as an image name")
	}
	first, _ := openTask(t, s, 1, store.TaskClone)
	if _, err := m.Start(ctx, first.ID, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Start(ctx, first.ID+1, "win11", mac); !errors.Is(err, ErrBusy) {
		t.Fatalf("second clone of one image: %v", err)
	}
	if _, err := m.Start(ctx, first.ID+2, "other", mac); err != nil {
		t.Fatalf("clone of another image: %v", err)
	}
	m.Abort(first.ID)
	m.Abort(first.ID + 2)
	if err := s.SetSetting(ctx, store.SettingImageDir, ""); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := m.Start(ctx, 4, "win11", mac); !errors.Is(err, ErrNoImageDir) {
		t.Fatalf("no directory: %v", err)
	}
}

func TestRestartDropsThePartialFile(t *testing.T) {
	m, s, dir := setup(t)
	if _, err := m.Start(ctx, 1, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h, payload, _ := proto.EncodeBlock(block(1))
	body := proto.AppendPut(nil, proto.PutRecord{Partition: 3, Position: 0, Hash: h.Hash, Block: &h, Payload: payload})
	if err := m.Put(1, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, _ := clone(t, m, 1, map[uint32][]byte{0: block(1)}) // the agent starts over
	if res.File != "v1.ldisk" {
		t.Fatalf("result: %+v", res)
	}
	if images, _ := s.Images(ctx); len(images[0].Versions) != 1 {
		t.Fatalf("images: %+v", images)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, incomingDir)); len(entries) != 0 {
		t.Fatalf("left in .incoming: %v", entries)
	}
}

func TestInterruptedCloneLeavesNoVersion(t *testing.T) {
	m, s, dir := setup(t)
	clone(t, m, 1, map[uint32][]byte{0: block(1)})

	if _, err := m.Start(ctx, 2, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.Abort(2)

	if latest, _ := s.LatestVersion(ctx, "win11"); latest == nil || latest.Number != 1 {
		t.Fatalf("latest: %+v", latest)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "win11")); len(entries) != 1 {
		t.Fatalf("files: %v", entries)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, incomingDir)); len(entries) != 0 {
		t.Fatalf("left in .incoming: %v", entries)
	}
	if _, err := m.Check(2, nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Check after Abort: %v", err)
	}
}

func TestSilentCloneIsDropped(t *testing.T) {
	m, _, dir := setup(t)
	if _, err := m.Start(ctx, 1, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	m.sweep(time.Now())
	if _, err := m.Check(1, nil); err != nil {
		t.Fatalf("a working clone was dropped: %v", err)
	}

	m.sweep(time.Now().Add(idleLimit + time.Minute))
	if _, err := m.Check(1, nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("silent clone: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, incomingDir)); len(entries) != 0 {
		t.Fatalf("left in .incoming: %v", entries)
	}
	if _, err := m.Start(ctx, 2, "win11", mac); err != nil {
		t.Fatalf("the image stayed busy: %v", err)
	}
	m.Abort(2)
}

func TestRunDropsEverythingOnShutdown(t *testing.T) {
	m, _, dir := setup(t)
	if _, err := m.Start(ctx, 1, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	stop, cancel := context.WithCancel(ctx)
	cancel()
	m.Run(stop)
	if entries, _ := os.ReadDir(filepath.Join(dir, incomingDir)); len(entries) != 0 {
		t.Fatalf("left in .incoming: %v", entries)
	}
}

func TestBadManifestPublishesNothing(t *testing.T) {
	m, s, dir := setup(t)
	if _, err := m.Start(ctx, 1, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Finish(ctx, 1, proto.Manifest{}); err == nil {
		t.Fatal("Finish accepted an empty manifest")
	}
	if images, _ := s.Images(ctx); len(images) != 0 {
		t.Fatalf("images: %+v", images)
	}
	if _, err := os.Stat(filepath.Join(dir, "win11", "v1.ldisk")); err == nil {
		t.Fatal("a failed clone was published")
	}
}

func TestPutRejectsDamage(t *testing.T) {
	m, _, _ := setup(t)
	t.Cleanup(func() { m.Abort(1) })
	if _, err := m.Start(ctx, 1, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	h, payload, _ := proto.EncodeBlock(block(1))

	flipped := bytes.Clone(payload)
	flipped[0] ^= 0xff
	damaged := proto.AppendPut(nil, proto.PutRecord{Partition: 3, Hash: h.Hash, Block: &h, Payload: flipped})
	if err := m.Put(1, bytes.NewReader(damaged)); !errors.Is(err, proto.ErrCorrupt) {
		t.Fatalf("damaged block: %v", err)
	}

	other := proto.HashBlock([]byte("x"))
	mismatch := proto.AppendPut(nil, proto.PutRecord{Partition: 3, Hash: other, Block: &h, Payload: payload})
	if err := m.Put(1, bytes.NewReader(mismatch)); !errors.Is(err, proto.ErrCorrupt) {
		t.Fatalf("mismatch: %v", err)
	}

	unknown := proto.AppendPut(nil, proto.PutRecord{Partition: 3, Hash: other})
	if err := m.Put(1, bytes.NewReader(unknown)); err == nil || !strings.Contains(err.Error(), "never written") {
		t.Fatalf("unknown block: %v", err)
	}

	whole := proto.AppendPut(nil, proto.PutRecord{Partition: 3, Hash: h.Hash, Block: &h, Payload: payload})
	if err := m.Put(1, bytes.NewReader(whole[:len(whole)-5])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("cut body: %v", err)
	}
}

// TestCleanIncomingRemovesWhatAStopLeft: a .part of a clone cut short by a stop
// or a power cut would otherwise fill the disk, in every place images live.
func TestCleanIncomingRemovesWhatAStopLeft(t *testing.T) {
	m, s, images := setup(t)
	other := filepath.Join(t.TempDir(), "old")
	if err := s.CreateImage(ctx, "moved", filepath.Join(other, "moved")); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	var parts []string
	for _, place := range []string{images, other} {
		dir := filepath.Join(place, incomingDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		part := filepath.Join(dir, "7.part")
		if err := os.WriteFile(part, []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		parts = append(parts, part)
	}
	kept := filepath.Join(images, incomingDir, "notes.txt")
	if err := os.WriteFile(kept, []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := m.CleanIncoming(ctx); err != nil {
		t.Fatalf("CleanIncoming: %v", err)
	}
	for _, part := range parts {
		if _, err := os.Stat(part); err == nil {
			t.Fatalf("%s is still there", part)
		}
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatalf("a file that is not a .part went: %v", err)
	}
}
