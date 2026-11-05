package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fogs/proto"
	"fogs/server/store"
)

// fakeTarget is a disk that records what the agent does to it.
type fakeTarget struct {
	blocks   map[uint64][]byte // partition 3, by offset
	checkErr error
	prepared bool
	finished bool
	closed   bool
	bootable bool
	bootErr  error
	grown    bool
}

func (d *fakeTarget) Describe() string                             { return "тестовый диск" }
func (d *fakeTarget) Fit(m proto.Manifest) (proto.Manifest, error) { return m, d.checkErr }
func (d *fakeTarget) Grow(proto.Manifest) error                    { d.grown = true; return nil }
func (d *fakeTarget) Prepare(proto.Manifest) error                 { d.prepared = true; return nil }
func (d *fakeTarget) Finish() error                                { d.finished = true; return nil }
func (d *fakeTarget) Close() error                                 { d.closed = true; return nil }
func (d *fakeTarget) MakeBootable(proto.Manifest) error {
	if !d.closed {
		return errors.New("bootable before the disk was released")
	}
	d.bootable = true
	return d.bootErr
}

func (d *fakeTarget) WriteBlock(partition uint32, offset uint64, data []byte) error {
	if !d.prepared {
		return errors.New("write before Prepare")
	}
	if partition != 3 {
		return errors.New("write to a partition without content")
	}
	d.blocks[offset] = bytes.Clone(data)
	return nil
}

func (e *cloneEnv) deploy(t *testing.T, d *fakeTarget) (*worker, error) {
	t.Helper()
	if _, err := e.s.CreateTask(context.Background(), e.host.ID, store.TaskDeploy, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w, _ := testWorker(e.url)
	w.openTarget = func() (target, error) { return d, nil }
	return w, w.attempt()
}

// TestDeployRestoresTheDisk is the promise of the whole project: what was cloned
// is what lands on the other PC, through a chain of versions.
func TestDeployRestoresTheDisk(t *testing.T) {
	e := newCloneEnv(t)
	size := uint64(50*testBlock + 4096)
	v1 := map[uint32][]byte{}
	for pos := range uint32(cloneBatch + 10) {
		v1[pos] = fill(byte(pos), testBlock)
	}
	v1[50] = fill(99, 4096) // the short last block
	e.run(t, &fakeDisk{blocks: v1, size: size})

	v2 := map[uint32][]byte{}
	for pos, raw := range v1 {
		v2[pos] = raw
	}
	v2[3] = fill(201, testBlock)
	v2[4] = make([]byte, testBlock) // used, and all zeros
	delete(v2, 5)                   // became free
	e.run(t, &fakeDisk{blocks: v2, size: size})

	stale := fill(222, testBlock)
	d := &fakeTarget{blocks: map[uint64][]byte{
		4 * testBlock:  stale, // must be overwritten with zeros
		5 * testBlock:  stale, // free in the image: stays as it was
		45 * testBlock: stale, // free in the image: stays as it was
	}}
	w, err := e.deploy(t, d)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if st := w.screen.snapshot(); st.Title != "Готово" {
		t.Fatalf("screen: %+v", st)
	}
	if !d.finished || !d.closed || !d.bootable || !d.grown {
		t.Fatalf("target: %+v", d)
	}

	for pos, raw := range v2 {
		got := d.blocks[uint64(pos)*testBlock]
		if !bytes.Equal(got, raw) {
			t.Fatalf("block %d differs: %d bytes, want %d", pos, len(got), len(raw))
		}
	}
	if !bytes.Equal(d.blocks[5*testBlock], stale) || !bytes.Equal(d.blocks[45*testBlock], stale) {
		t.Fatal("a free block was written")
	}

	ctx := context.Background()
	hosts, _ := e.s.Hosts(ctx)
	if hosts[0].Task != nil || hosts[0].LastTask == nil || hosts[0].LastTask.Status != store.StatusDone {
		t.Fatalf("host: %+v", hosts[0])
	}
	if ok, _ := e.s.HasTask(ctx, testMAC); ok {
		t.Fatal("the PC would boot WinPE again")
	}
}

func TestEscBeforeDeployLeavesTheDiskAlone(t *testing.T) {
	e := newCloneEnv(t)
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})

	ctx := context.Background()
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskDeploy, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	d := &fakeTarget{blocks: map[uint64][]byte{}}
	w, _ := testWorker(e.url)
	w.openTarget = func() (target, error) { return d, nil }
	w.abort = make(chan struct{}, 1)
	w.abort <- struct{}{}
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if d.prepared || len(d.blocks) != 0 {
		t.Fatalf("the disk was touched: %+v", d)
	}
	hosts, _ := e.s.Hosts(ctx)
	if last := hosts[0].LastTask; hosts[0].Task != nil || last == nil || last.Status != store.StatusFailed {
		t.Fatalf("host: %+v", hosts[0])
	}
}

func TestDeployWarnsBeforeWriting(t *testing.T) {
	e := newCloneEnv(t)
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})
	if _, err := e.s.CreateTask(context.Background(), e.host.ID, store.TaskDeploy, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	d := &fakeTarget{blocks: map[uint64][]byte{}}
	w, _ := testWorker(e.url)
	w.openTarget = func() (target, error) { return d, nil }
	var titles []string
	w.sleep = func(time.Duration) {
		if !d.prepared {
			st := w.screen.snapshot()
			titles = append(titles, st.Title+"|"+st.Hint)
		}
	}
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	warned := 0
	for _, s := range titles {
		if strings.HasPrefix(s, "Диск будет стёрт|Esc") {
			warned++
		}
	}
	if warned != confirmSeconds {
		t.Fatalf("warned for %d seconds, want %d: %v", warned, confirmSeconds, titles)
	}
}

func TestDeployRefusedDiskIsNotTouched(t *testing.T) {
	e := newCloneEnv(t)
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})
	d := &fakeTarget{blocks: map[uint64][]byte{}, checkErr: errors.New("образу нужно больше места")}
	_, err := e.deploy(t, d)
	if err == nil || !strings.Contains(err.Error(), "больше места") {
		t.Fatalf("got %v", err)
	}
	if d.prepared {
		t.Fatal("Prepare ran after Fit failed")
	}
}

func TestDamagedImageStopsTheDeploy(t *testing.T) {
	e := newCloneEnv(t)
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock), 1: fill(2, testBlock)}, size: 2 * testBlock})

	path := filepath.Join(e.images, "win11", "v1.ldisk")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	b[proto.HeaderSize+proto.BlockHeaderSize+100] ^= 0xff
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	d := &fakeTarget{blocks: map[uint64][]byte{}}
	_, err = e.deploy(t, d)
	if !errors.Is(err, proto.ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
	if d.finished {
		t.Fatal("a damaged image was reported as finished")
	}
}

// TestBootEntryFailureIsNotADeployFailure: the data is already on the disk.
func TestBootEntryFailureIsNotADeployFailure(t *testing.T) {
	e := newCloneEnv(t)
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})
	d := &fakeTarget{blocks: map[uint64][]byte{}, bootErr: errors.New("bcdboot: нет прав")}
	w, err := e.deploy(t, d)
	if err != nil {
		t.Fatalf("attempt: %v", err)
	}
	st := w.screen.snapshot()
	if st.Title != "Готово" || !strings.Contains(strings.Join(st.Log, "\n"), "нет прав") {
		t.Fatalf("screen: %+v", st)
	}
}

func TestChooseTarget(t *testing.T) {
	data := diskInfo{number: 0, internal: true}
	boot := diskInfo{number: 1, internal: true, hasESP: true}
	blank := diskInfo{number: 2, internal: true, empty: true}
	blank2 := diskInfo{number: 3, internal: true, empty: true}
	usb := diskInfo{number: 4, empty: true}
	usbBoot := diskInfo{number: 5, hasESP: true}
	for _, c := range []struct {
		name  string
		disks []diskInfo
		want  int // -1: refused
	}{
		{"boot disk wins over a data disk", []diskInfo{data, boot}, 1},
		{"boot disk wins over a blank one", []diskInfo{blank, boot}, 1},
		{"a new disk alone", []diskInfo{blank}, 2},
		{"a foreign disk alone", []diskInfo{data}, 0},
		{"a USB stick is not a disk of the PC", []diskInfo{usbBoot, blank}, 2},
		{"a new disk next to a data disk", []diskInfo{data, blank}, 2},
		{"two new disks", []diskInfo{blank, blank2}, -1},
		{"two data disks", []diskInfo{data, {number: 7, internal: true}}, -1},
		{"only USB", []diskInfo{usb, usbBoot}, -1},
		{"nothing", nil, -1},
	} {
		got, err := chooseTarget(c.disks)
		if c.want < 0 {
			if err == nil {
				t.Errorf("%s: chose %d", c.name, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: %d, %v", c.name, got, err)
		}
	}
}

func TestGPTArea(t *testing.T) {
	const size = 128 << 30
	for sector, want := range map[uint32][2]uint64{
		512:  {34 * 512, size - 33*512},
		4096: {6 * 4096, size - 5*4096},
	} {
		first, end := gptArea(size, sector)
		if first != want[0] || end != want[1] {
			t.Errorf("sector %d: %d, %d", sector, first, end)
		}
	}
}

// TestDeployForgetsTheSavedTable: the volume the table was saved for is overwritten.
func TestDeployForgetsTheSavedTable(t *testing.T) {
	e := newCloneEnv(t)
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})
	if err := e.s.SetSavedTable(context.Background(), testHostMAC(t, e), []byte("table")); err != nil {
		t.Fatalf("SetSavedTable: %v", err)
	}
	if _, err := e.deploy(t, &fakeTarget{blocks: map[uint64][]byte{}}); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	if table := savedTable(t, e); table != nil {
		t.Fatalf("table: %q", table)
	}
}

// otherPC is a second registered PC with a task and a client of its own.
func otherPC(t *testing.T, e *cloneEnv, kind string) (*client, store.Task, net.HardwareAddr) {
	t.Helper()
	ctx := context.Background()
	mac := net.HardwareAddr{2, 0, 0, 0, 0, 7}
	other, err := e.s.AddHost(ctx, mac, "PC-07")
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	task, err := e.s.CreateTask(ctx, other.ID, kind, "win11")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return newClient(proto.AgentConfig{Server: e.url, MAC: mac.String()}), task, mac
}

// TestDeployRunsDuringAClone: a clone in progress does not hold a deploy up.
func TestDeployRunsDuringAClone(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	e.run(t, &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock})
	c, task, _ := otherPC(t, e, store.TaskClone)
	if _, err := c.cloneStart(task.ID); err != nil {
		t.Fatalf("cloneStart: %v", err)
	}
	defer c.send(task.ID, "abort", "application/json", nil)

	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskDeploy, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	d := &fakeTarget{blocks: map[uint64][]byte{}}
	w, _ := testWorker(e.url)
	w.openTarget = func() (target, error) { return d, nil }
	w.sleep = func(wait time.Duration) {
		if wait == imageWait {
			t.Error("the deploy waited for a clone")
		}
	}
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if !d.prepared {
		t.Fatal("nothing was deployed")
	}
}

// TestSecondCloneWaitsBeforeItShrinks: the volume stays whole for the whole wait.
func TestSecondCloneWaitsBeforeItShrinks(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	c, task, mac := otherPC(t, e, store.TaskClone)
	if err := c.cloneClaim(task.ID); err != nil {
		t.Fatalf("cloneClaim: %v", err)
	}
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	d := &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	waits := 0
	w.sleep = func(wait time.Duration) {
		if wait != imageWait {
			return
		}
		if d.shrunk {
			t.Error("the volume was shrunk while waiting")
		}
		if waits++; waits == 2 {
			if err := e.s.FinishTask(ctx, mac, task.ID, true, "готово"); err != nil {
				t.Errorf("FinishTask: %v", err)
			}
		}
	}
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if waits != 2 || !d.shrunk {
		t.Fatalf("waits %d, shrunk %v", waits, d.shrunk)
	}
}

// TestCancelEndsTheWait: the operator can always take a waiting task back.
func TestCancelEndsTheWait(t *testing.T) {
	e := newCloneEnv(t)
	ctx := context.Background()
	c, task, _ := otherPC(t, e, store.TaskClone)
	if err := c.cloneClaim(task.ID); err != nil {
		t.Fatalf("cloneClaim: %v", err)
	}
	if _, err := e.s.CreateTask(ctx, e.host.ID, store.TaskClone, "win11"); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	d := &fakeDisk{blocks: map[uint32][]byte{0: fill(1, testBlock)}, size: testBlock}
	w, _ := testWorker(e.url)
	w.openDisk = func() (source, error) { return d, nil }
	w.sleep = func(time.Duration) {
		if err := e.s.CancelTask(ctx, e.host.ID); err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Errorf("CancelTask: %v", err)
		}
	}
	if err := w.attempt(); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	if d.shrunk {
		t.Fatal("a cancelled clone touched the disk")
	}
}
