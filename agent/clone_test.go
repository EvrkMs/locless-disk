package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"fogs/proto"
	"fogs/server/store"
)

const (
	testGUID  = "12345678-9abc-def0-1234-56789abcdef0"
	testBlock = proto.DefaultBlockSize
)

// fakeDisk is a partition 3 of blocks, some of them empty, and an MSR-like
// partition 2 that holds nothing.
type fakeDisk struct {
	blocks           map[uint32][]byte
	size             uint64
	closed           bool
	shrunk, restored bool
	fail             bool
	noShrink         bool  // Shrink finds nothing to do
	repairFails      int   // how many times Repair fails before it works
	repairErr        error // what it fails with, a plain error by default
	repairs          int
}

func (d *fakeDisk) Describe() string { return "тестовый диск" }

func (d *fakeDisk) Manifest() proto.Manifest {
	return proto.Manifest{
		Version: proto.ManifestVersion, DiskSize: d.size + 64<<20, SectorSize: 512, DiskGUID: testGUID,
		Partitions: []proto.Partition{
			{Number: 2, TypeGUID: testGUID, GUID: testGUID, Offset: 1 << 20, Size: 16 << 20, Policy: proto.PolicyFixed, Content: proto.ContentNone},
			{Number: 3, TypeGUID: testGUID, GUID: testGUID, Offset: 32 << 20, Size: d.size, Policy: proto.PolicyFill, Content: proto.ContentBlocks},
		},
	}
}

func (d *fakeDisk) UsedBlocks(uint32, uint32) uint64 { return uint64(len(d.blocks)) }

func (d *fakeDisk) ReadBlock(partition uint32, offset uint64, dst []byte) (bool, error) {
	if d.fail {
		return false, errors.New("bad sector")
	}
	raw, ok := d.blocks[uint32(offset/testBlock)]
	if !ok {
		return false, nil
	}
	copy(dst, raw)
	return true, nil
}

func (d *fakeDisk) Table() ([]byte, error) { return []byte("table"), nil }

func (d *fakeDisk) Shrink(log func(string)) bool {
	d.shrunk = !d.noShrink
	if d.shrunk {
		log("том сжат: 100 МБ → 40 МБ")
	} else {
		log("сжатие: том уже почти полон, сжимать нечего")
	}
	return d.shrunk
}

func (d *fakeDisk) Repair(table []byte) error {
	d.repairs++
	if string(table) != "table" {
		return errors.New("wrong table")
	}
	if d.repairFails > 0 {
		d.repairFails--
		if d.repairErr != nil {
			return d.repairErr
		}
		return errors.New("том занят")
	}
	d.restored = true
	return nil
}

func (d *fakeDisk) Close() error {
	d.closed = true
	return nil
}

func fill(seed byte, n int) []byte {
	b := make([]byte, n)
	x := uint32(seed)*2654435761 + 1
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// cloneEnv is a real server with an image directory and a clone task to run.
type cloneEnv struct {
	url    string
	s      *store.Store
	host   store.Host
	images string
}

func newCloneEnv(t *testing.T) *cloneEnv {
	t.Helper()
	srv, s, host := realServer(t)
	images := filepath.Join(t.TempDir(), "images")
	if err := s.SetSetting(context.Background(), store.SettingImageDir, images); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	return &cloneEnv{srv.URL, s, host, images}
}

func (e *cloneEnv) run(t *testing.T, d *fakeDisk) *worker {
	t.Helper()
	if _, err := e.s.CreateTask(context.Background(), e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	return w
}

// restore reads a chain back from the files the server wrote.
func (e *cloneEnv) restore(t *testing.T, names ...string) map[uint32][]byte {
	t.Helper()
	var files []*proto.File
	for _, name := range names {
		f, err := os.Open(filepath.Join(e.images, "win11", name))
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
	got := map[uint32][]byte{}
	for _, entry := range c.Index().Entries {
		raw, err := c.Read(entry)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got[entry.Position] = raw
	}
	return got
}

func sameBlocks(t *testing.T, got, want map[uint32][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d blocks, want %d", len(got), len(want))
	}
	for pos, raw := range want {
		if !bytes.Equal(got[pos], raw) {
			t.Fatalf("block %d differs", pos)
		}
	}
}

// TestCloneThenDeltaRestoresTheDisk goes through the whole path: agent, HTTP,
// the real server, files on disk, and back to the original bytes.
func TestCloneThenDeltaRestoresTheDisk(t *testing.T) {
	e := newCloneEnv(t)
	size := uint64(100*testBlock + 4096) // the last block is short
	blocks := map[uint32][]byte{}
	for pos := range uint32(cloneBatch*2 + 5) { // several batches
		blocks[pos] = fill(byte(pos), testBlock)
	}
	blocks[100] = fill(200, 4096)
	d := &fakeDisk{blocks: blocks, size: size}

	w := e.run(t, d)
	if st := w.screen.snapshot(); st.Title != "Готово" || !strings.Contains(st.Detail, "v1.ldisk") {
		t.Fatalf("screen: %+v", st)
	}
	if !d.closed {
		t.Fatal("disk was left open")
	}
	sameBlocks(t, e.restore(t, "v1.ldisk"), blocks)

	ctx := context.Background()
	hosts, _ := e.s.Hosts(ctx)
	if hosts[0].Task != nil || hosts[0].LastTask == nil || hosts[0].LastTask.Status != store.StatusDone {
		t.Fatalf("host: %+v", hosts[0])
	}

	// The second clone of a slightly changed disk stores a small delta.
	next := map[uint32][]byte{}
	for pos, raw := range blocks {
		next[pos] = raw
	}
	next[7] = fill(250, testBlock)
	delete(next, 8)
	e.run(t, &fakeDisk{blocks: next, size: size})
	sameBlocks(t, e.restore(t, "v1.ldisk", "v2.ldelta"), next)

	images, err := e.s.Images(ctx)
	if err != nil || len(images) != 1 || len(images[0].Versions) != 2 {
		t.Fatalf("images %+v, err %v", images, err)
	}
	if delta, full := images[0].Versions[1].Size, images[0].Versions[0].Size; delta > full/10 {
		t.Fatalf("delta is %d bytes of %d", delta, full)
	}
}

func TestCloneStopsOnReadError(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) {
		return &fakeDisk{blocks: map[uint32][]byte{0: {1}}, size: testBlock, fail: true}, nil
	}
	err := w.attempt()
	if err == nil || !strings.Contains(err.Error(), "bad sector") {
		t.Fatalf("got %v", err)
	}
	if images, _ := e.s.Images(ctx); len(images) != 0 {
		t.Fatalf("a failed clone made an image: %+v", images)
	}
}

func TestCloneNoDisk(t *testing.T) {
	e := newCloneEnv(t)
	if _, err := e.s.CreateTask(context.Background(), e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return nil, errors.New("нет диска") }
	if err := w.attempt(); err == nil || !strings.Contains(err.Error(), "нет диска") {
		t.Fatalf("got %v", err)
	}
}

// TestCancelStopsTheClone uses a disk that cancels the task while it is read.
func TestCancelStopsTheClone(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	blocks := map[uint32][]byte{}
	for pos := range uint32(cloneBatch * 3) {
		blocks[pos] = fill(byte(pos), testBlock)
	}
	d := &cancellingDisk{fakeDisk: fakeDisk{blocks: blocks, size: uint64(len(blocks)) * testBlock}}
	d.cancel = func() {
		if err := e.s.CancelTask(ctx, e.host.ID); err != nil {
			t.Errorf("CancelTask: %v", err)
		}
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if st := w.screen.snapshot(); st.Title != "Задача отменена" {
		t.Fatalf("title: %q", st.Title)
	}
	if images, _ := e.s.Images(ctx); len(images) != 0 {
		t.Fatalf("a cancelled clone made an image: %+v", images)
	}
	if entries, _ := os.ReadDir(filepath.Join(e.images, ".incoming")); len(entries) != 0 {
		t.Fatalf("left in .incoming: %v", entries)
	}
}

// cancellingDisk cancels the task after the first batch was read.
type cancellingDisk struct {
	fakeDisk
	reads  int
	cancel func()
}

func (d *cancellingDisk) ReadBlock(p uint32, offset uint64, dst []byte) (bool, error) {
	if d.reads++; d.reads == cloneBatch+1 {
		d.cancel()
	}
	return d.fakeDisk.ReadBlock(p, offset, dst)
}

// TestOnlyMissingBlocksTravel checks the point of the check step on the wire.
func TestOnlyMissingBlocksTravel(t *testing.T) {
	var bytesPut atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check"):
			w.Write(make([]byte, r.ContentLength/32)) // the server has everything
		case strings.HasSuffix(r.URL.Path, "/put"):
			bytesPut.Add(r.ContentLength)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	blocks := map[uint32][]byte{0: fill(1, testBlock), 1: fill(2, testBlock)}
	worker, _ := testWorker(srv.URL)
	c := cloneRun{w: worker, src: &fakeDisk{blocks: blocks, size: 2 * testBlock}, task: 1, blockSize: testBlock, started: time.Now()}
	worker.names = cloneStages
	if err := c.readDisk(); err != nil {
		t.Fatalf("readDisk: %v", err)
	}
	if bytesPut.Load() > 4096 {
		t.Fatalf("%d bytes sent for blocks the server has", bytesPut.Load())
	}
	if c.sent == 0 || c.read != 2*testBlock {
		t.Fatalf("read %d, sent %d", c.read, c.sent)
	}
}

// countingDisk counts reads from any goroutine.
type countingDisk struct {
	fakeDisk
	reads atomic.Int64
}

func (d *countingDisk) ReadBlock(p uint32, offset uint64, dst []byte) (bool, error) {
	d.reads.Add(1)
	return d.fakeDisk.ReadBlock(p, offset, dst)
}

// TestFailedSendEndsTheReading: right after a failed capture the volume gets its size
// back, so the reader must be gone by then and stop early, not at the end of the disk.
func TestFailedSendEndsTheReading(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "нет места", http.StatusInternalServerError)
	}))
	defer srv.Close()

	blocks := map[uint32][]byte{}
	for pos := range uint32(cloneBatch * (pipelineSlabs + 3)) {
		blocks[pos] = fill(byte(pos), testBlock)
	}
	d := &countingDisk{fakeDisk: fakeDisk{blocks: blocks, size: uint64(len(blocks)) * testBlock}}
	worker, _ := testWorker(srv.URL)
	c := cloneRun{w: worker, src: d, task: 1, blockSize: testBlock, started: time.Now()}
	worker.names = cloneStages
	if err := c.readDisk(); err == nil {
		t.Fatal("readDisk: no error")
	}
	reads := d.reads.Load()
	if reads >= int64(len(blocks)) {
		t.Fatalf("read all %d blocks after the first batch failed", reads)
	}
	time.Sleep(20 * time.Millisecond)
	if d.reads.Load() != reads {
		t.Fatal("the disk was read after readDisk returned")
	}
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[uint64]string{
		2048:          "2 КБ",
		5 << 20:       "5 МБ",
		3 << 30:       "3.0 ГБ",
		3<<30 + 1<<29: "3.5 ГБ",
	} {
		if got := formatBytes(n); got != want {
			t.Fatalf("%d: got %q, want %q", n, got, want)
		}
	}
}

// TestCloneShrinksAndRestores follows FOG: the volume is shrunk for the capture
// and given back afterwards, also when the clone fails.
func TestCloneShrinksAndRestores(t *testing.T) {
	e := newCloneEnv(t)
	d := &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock}
	e.run(t, d)
	if !d.shrunk || !d.restored {
		t.Fatalf("shrunk %v, restored %v", d.shrunk, d.restored)
	}

	failing := &fakeDisk{blocks: map[uint32][]byte{0: {1}}, size: testBlock, fail: true}
	if _, err := e.s.CreateTask(context.Background(), e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return failing, nil }
	if err := w.attempt(); err == nil {
		t.Fatal("attempt: no error")
	}
	if !failing.restored {
		t.Fatal("a failed clone left the volume shrunk")
	}
}

// TestErrorsReachTheServer: a failure must be visible without walking to the PC.
func TestErrorsReachTheServer(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return nil, errors.New("диск пропал") }
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hosts, _ := e.s.Hosts(ctx)
		if tk := hosts[0].Task; tk != nil && tk.Stage == proto.StageFailed {
			if !strings.Contains(tk.Message, "диск пропал") {
				t.Fatalf("message: %q", tk.Message)
			}
			// The failure brings how far the task got.
			if m := lastMetrics(t, e.s); !slices.Equal(names(m.Steps), []string{"Открытие диска"}) {
				t.Fatalf("metrics of the failure: %+v", m)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the error never reached the server")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := e.s.CancelTask(ctx, e.host.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	w.retry <- struct{}{} // the task is gone, so this ends the run
	<-done
}

func savedTable(t *testing.T, e *cloneEnv) []byte {
	t.Helper()
	table, err := e.s.SavedTable(context.Background(), testHostMAC(t, e))
	if err != nil {
		t.Fatalf("SavedTable: %v", err)
	}
	return table
}

func testHostMAC(t *testing.T, e *cloneEnv) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(e.host.MAC)
	if err != nil {
		t.Fatalf("ParseMAC: %v", err)
	}
	return mac
}

// TestVolumeComesBackBeforeTheResult: a clone is not "done" until the volume is back,
// and a busy volume gets more tries.
func TestVolumeComesBackBeforeTheResult(t *testing.T) {
	e := newCloneEnv(t)
	d := &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock, repairFails: 2}
	e.run(t, d)
	if d.repairs != 3 || !d.restored {
		t.Fatalf("repairs %d, restored %v", d.repairs, d.restored)
	}
	if table := savedTable(t, e); table != nil {
		t.Fatalf("the table stayed on the server: %q", table)
	}
}

// TestLostVolumeIsReportedAndRepairedLater: the image is good and stays, the task
// fails, and the next task of the PC, of any kind, finishes the job first.
func TestLostVolumeIsReportedAndRepairedLater(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	d := &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock, repairFails: 100}
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	err := w.attempt()
	if err == nil || !strings.Contains(err.Error(), "том эталона не восстановлен") {
		t.Fatalf("attempt: %v", err)
	}
	if d.repairs != restoreTries {
		t.Fatalf("repairs %d", d.repairs)
	}
	hosts, _ := e.s.Hosts(ctx)
	h := hosts[0]
	if h.Task != nil || h.LastTask == nil || h.LastTask.Status != store.StatusFailed || !h.Shrunk {
		t.Fatalf("host: %+v, last: %+v", h, h.LastTask)
	}
	if !strings.Contains(h.LastTask.Message, "образ сохранён") {
		t.Fatalf("message: %q", h.LastTask.Message)
	}
	if _, err := os.Stat(filepath.Join(e.images, "win11", "v1.ldisk")); err != nil {
		t.Fatalf("the image is gone: %v", err)
	}

	d.repairFails = 0
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskAgent, ""); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ = testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if !d.restored || savedTable(t, e) != nil {
		t.Fatalf("restored %v, table %q", d.restored, savedTable(t, e))
	}
}

// TestStaleTableIsDropped: a table that no longer fits is not an error, and is
// never tried again.
func TestStaleTableIsDropped(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	if err := e.s.SetSavedTable(ctx, testHostMAC(t, e), []byte("table")); err != nil {
		t.Fatalf("SetSavedTable: %v", err)
	}
	d := &fakeDisk{repairFails: 1, repairErr: errStaleTable}
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskAgent, ""); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if d.repairs != 1 || savedTable(t, e) != nil {
		t.Fatalf("repairs %d, table %q", d.repairs, savedTable(t, e))
	}
}

// TestNoShrinkLeavesNoTable: without a shrink there is nothing to give back.
func TestNoShrinkLeavesNoTable(t *testing.T) {
	e := newCloneEnv(t)
	d := &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock, noShrink: true}
	e.run(t, d)
	if d.repairs != 0 || savedTable(t, e) != nil {
		t.Fatalf("repairs %d, table %q", d.repairs, savedTable(t, e))
	}
}

// crashingDisk has a bug in the middle of a clone.
type crashingDisk struct{ fakeDisk }

func (d *crashingDisk) ReadBlock(uint32, uint64, []byte) (bool, error) {
	panic("ошибка в коде")
}

// TestCrashStillGivesTheVolumeBack: a panic in a clone ends as an ordinary error,
// after the volume is back, and reaches the server with its stack.
func TestCrashStillGivesTheVolumeBack(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	d := &crashingDisk{fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock}}
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.run()
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		hosts, _ := e.s.Hosts(ctx)
		if tk := hosts[0].Task; tk != nil && tk.Stage == proto.StageFailed {
			if !strings.Contains(tk.Message, "ошибка в коде") {
				t.Fatalf("message: %q", tk.Message)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the crash never reached the server")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !d.restored {
		t.Fatal("the crash left the volume shrunk")
	}
	if err := e.s.CancelTask(ctx, e.host.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	w.retry <- struct{}{} // the task is gone, so this ends the run
	<-done
}
