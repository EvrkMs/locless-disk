package imaging

import (
	"bytes"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fogs/proto"
	"fogs/server/store"
)

// disk reads what a chain says the disk holds.
func disk(t *testing.T, c *proto.Chain) map[uint32][]byte {
	t.Helper()
	got := map[uint32][]byte{}
	for _, e := range c.Index().Entries {
		raw, err := c.Read(e)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		got[e.Position] = raw
	}
	return got
}

func sameDisk(t *testing.T, got, want map[uint32][]byte) {
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

func files(t *testing.T, s *store.Store) []string {
	t.Helper()
	versions, err := s.Versions(ctx, "win11")
	if err != nil {
		t.Fatalf("Versions: %v", err)
	}
	names := []string{}
	for _, v := range versions {
		names = append(names, v.File)
	}
	return names
}

// threeVersions clones v1, v2 with a changed block, and v3 with a deleted one.
func threeVersions(t *testing.T, m *Manager) (v1, v2, v3 map[uint32][]byte) {
	t.Helper()
	v1 = map[uint32][]byte{0: block(1), 1: block(2), 2: block(3)}
	v2 = map[uint32][]byte{0: block(1), 1: block(4), 2: block(3)}
	v3 = map[uint32][]byte{0: block(1), 1: block(4)}
	clone(t, m, 1, v1)
	clone(t, m, 2, v2)
	clone(t, m, 3, v3)
	return
}

func TestRestoreSwitchesOffAndEnableBringsBack(t *testing.T) {
	m, s, dir := setup(t)
	v1, _, v3 := threeVersions(t, m)

	if err := m.Restore(ctx, "win11", 1); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := files(t, s); len(got) != 1 || got[0] != "v1.ldisk" {
		t.Fatalf("chain: %v", got)
	}
	for _, name := range []string{"v2.ldelta", "v3.ldelta"} {
		if _, err := os.Stat(filepath.Join(dir, "win11", name)); err != nil {
			t.Fatalf("a restore removed %s: %v", name, err)
		}
	}
	sameDisk(t, disk(t, testChain(t, dir, files(t, s)...)), v1)

	// Bringing back the newest brings back what it stands on.
	if err := m.Enable(ctx, "win11", 3); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if got := files(t, s); len(got) != 3 {
		t.Fatalf("chain: %v", got)
	}
	sameDisk(t, disk(t, testChain(t, dir, files(t, s)...)), v3)
}

// TestCloneAfterRestoreForks: a new clone goes on from the restored version, and the
// switched off one can no longer come back into this chain.
func TestCloneAfterRestoreForks(t *testing.T) {
	m, s, dir := setup(t)
	threeVersions(t, m)
	if err := m.Restore(ctx, "win11", 1); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	next := map[uint32][]byte{0: block(9), 1: block(2)}
	res, _ := clone(t, m, 4, next)
	if res.File != "v4.ldelta" {
		t.Fatalf("file %s", res.File)
	}
	if got := files(t, s); len(got) != 2 || got[1] != "v4.ldelta" {
		t.Fatalf("chain: %v", got)
	}
	sameDisk(t, disk(t, testChain(t, dir, files(t, s)...)), next)
	if err := m.Enable(ctx, "win11", 3); !errors.Is(err, ErrForked) {
		t.Fatalf("Enable: %v", err)
	}
}

// TestEnableChecksTheFiles: the order lives in the files, so two deltas swapped on
// disk are caught whatever the database says.
func TestEnableChecksTheFiles(t *testing.T) {
	m, s, dir := setup(t)
	threeVersions(t, m)
	if err := m.Restore(ctx, "win11", 1); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	a, b := filepath.Join(dir, "win11", "v2.ldelta"), filepath.Join(dir, "win11", "v3.ldelta")
	for _, step := range [][2]string{{a, a + ".tmp"}, {b, a}, {a + ".tmp", b}} {
		if err := os.Rename(step[0], step[1]); err != nil {
			t.Fatalf("Rename: %v", err)
		}
	}
	if err := m.Enable(ctx, "win11", 3); !errors.Is(err, ErrBrokenChain) {
		t.Fatalf("Enable: %v", err)
	}
	if got := files(t, s); len(got) != 1 {
		t.Fatalf("chain changed: %v", got)
	}
}

func waitMerge(t *testing.T, m *Manager) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		running, failure := m.Merging("win11")
		if failure != "" {
			t.Fatalf("merge failed: %s", failure)
		}
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("merge never ended")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMergeFoldsTheChain(t *testing.T) {
	m, s, dir := setup(t)
	_, _, v3 := threeVersions(t, m)
	if err := m.Restore(ctx, "win11", 2); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := m.Enable(ctx, "win11", 3); err != nil {
		t.Fatalf("Enable: %v", err)
	}
	if err := m.StartMerge(ctx, "win11"); err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	waitMerge(t, m)

	if got := files(t, s); len(got) != 1 || got[0] != "v4.ldisk" {
		t.Fatalf("chain: %v", got)
	}
	sameDisk(t, disk(t, testChain(t, dir, "v4.ldisk")), v3)
	entries, _ := os.ReadDir(filepath.Join(dir, "win11"))
	if len(entries) != 1 {
		t.Fatalf("files left: %v", entries)
	}

	// The merged file holds only what the disk needs, and clones go on from it.
	if err := m.StartMerge(ctx, "win11"); !errors.Is(err, ErrNothingToMerge) {
		t.Fatalf("second merge: %v", err)
	}
	next := map[uint32][]byte{0: block(1), 1: block(5)}
	res, sent := clone(t, m, 5, next)
	if res.File != "v5.ldelta" || sent != 1 {
		t.Fatalf("file %s, sent %d", res.File, sent)
	}
	sameDisk(t, disk(t, testChain(t, dir, "v4.ldisk", "v5.ldelta")), next)
}

// TestMergeDropsSwitchedOffVersions: they stand on files that are going away.
func TestMergeDropsSwitchedOffVersions(t *testing.T) {
	m, s, dir := setup(t)
	_, v2, _ := threeVersions(t, m)
	if err := m.Restore(ctx, "win11", 2); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if err := m.StartMerge(ctx, "win11"); err != nil {
		t.Fatalf("StartMerge: %v", err)
	}
	waitMerge(t, m)
	all, err := s.AllVersions(ctx, "win11")
	if err != nil || len(all) != 1 {
		t.Fatalf("versions: %+v, %v", all, err)
	}
	sameDisk(t, disk(t, testChain(t, dir, all[0].File)), v2)
}

// TestDeployIgnoresAnUnfinishedClone: while a clone is being received, a deploy gets
// the last finished version, and the file in progress is nowhere in its chain.
func TestDeployIgnoresAnUnfinishedClone(t *testing.T) {
	m, _, _ := setup(t)
	clone(t, m, 1, map[uint32][]byte{0: block(1)})
	if _, err := m.Start(ctx, 2, "win11", mac); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer m.Abort(2)
	h, payload, _ := proto.EncodeBlock(block(2))
	body := proto.AppendPut(nil, proto.PutRecord{Partition: 3, Position: 0, Hash: h.Hash, Block: &h, Payload: payload})
	if err := m.Put(2, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	info, err := m.StartDeploy(ctx, 3, "win11")
	if err != nil {
		t.Fatalf("StartDeploy: %v", err)
	}
	defer m.Abort(3)
	if info.Version != 1 {
		t.Fatalf("version %d", info.Version)
	}
}

// openTask makes a real unfinished task, as the turns look at the database.
func openTask(t *testing.T, s *store.Store, n byte, kind string) (store.Task, net.HardwareAddr) {
	t.Helper()
	address := net.HardwareAddr{2, 0, 0, 0, 0, n}
	host, err := s.AddHost(ctx, address, "PC")
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	task, err := s.CreateTask(ctx, host.ID, kind, "win11")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return task, address
}

func TestOneClonePerImage(t *testing.T) {
	m, s, _ := setup(t)
	first, address := openTask(t, s, 1, store.TaskClone)
	second, _ := openTask(t, s, 2, store.TaskClone)
	if err := m.Claim(ctx, first.ID, "win11"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := m.Claim(ctx, first.ID, "win11"); err != nil {
		t.Fatalf("Claim after a restart of the agent: %v", err)
	}
	if err := m.Claim(ctx, second.ID, "win11"); !errors.Is(err, ErrBusy) {
		t.Fatalf("second: %v", err)
	}
	if err := m.Claim(ctx, second.ID, "other"); err != nil {
		t.Fatalf("another image: %v", err)
	}
	if err := s.FinishTask(ctx, address, first.ID, true, ""); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	if err := m.Claim(ctx, second.ID, "win11"); err != nil {
		t.Fatalf("after the first ended: %v", err)
	}
}

func TestDeploysRunTogether(t *testing.T) {
	m, s, _ := setup(t)
	clone(t, m, 100, map[uint32][]byte{0: block(1)})
	first, _ := openTask(t, s, 1, store.TaskDeploy)
	second, _ := openTask(t, s, 2, store.TaskDeploy)
	for _, id := range []int64{first.ID, second.ID} {
		if _, err := m.StartDeploy(ctx, id, "win11"); err != nil {
			t.Fatalf("StartDeploy %d: %v", id, err)
		}
		defer m.Abort(id)
	}
}
