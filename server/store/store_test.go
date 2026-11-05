package store

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
)

var (
	ctx  = context.Background()
	macA = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01}
	macB = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x02}
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "fogs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addHost(t *testing.T, s *Store, mac net.HardwareAddr, name string) Host {
	t.Helper()
	h, err := s.AddHost(ctx, mac, name)
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	return h
}

func TestDataSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fogs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	addHost(t, s, macA, "PC-01")
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	hosts, err := s.Hosts(ctx)
	if err != nil || len(hosts) != 1 || hosts[0].Name != "PC-01" {
		t.Fatalf("hosts %+v, err %v", hosts, err)
	}
}

func TestSettings(t *testing.T) {
	s := open(t)
	if v, err := s.Setting(ctx, SettingImageDir); v != "" || err != nil {
		t.Fatalf("unset value: %q, err %v", v, err)
	}
	for _, want := range []string{`D:\images`, `E:\other`} {
		if err := s.SetSetting(ctx, SettingImageDir, want); err != nil {
			t.Fatalf("SetSetting: %v", err)
		}
		if v, err := s.Setting(ctx, SettingImageDir); v != want || err != nil {
			t.Fatalf("value %q, err %v", v, err)
		}
	}
}

// TestUpgradeKeepsData is the case of a database made by an older server, like
// the one already on the operator's disk.
func TestUpgradeKeepsData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fogs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	addHost(t, s, macA, "PC-01")
	if _, err := s.db.Exec("DROP TABLE settings; PRAGMA user_version = 1"); err != nil {
		t.Fatalf("downgrade: %v", err)
	}
	s.Close()

	s, err = Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	if hosts, _ := s.Hosts(ctx); len(hosts) != 1 {
		t.Fatalf("hosts: %+v", hosts)
	}
	if err := s.SetSetting(ctx, SettingImageDir, "x"); err != nil {
		t.Fatalf("SetSetting after upgrade: %v", err)
	}
}

func TestRefusesNewerDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fogs.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 999"); err != nil {
		t.Fatalf("set version: %v", err)
	}
	s.Close()
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("Open accepted a database from a newer server")
	}
}

func TestHostLifecycle(t *testing.T) {
	s := open(t)
	if ok, _ := s.IsHost(ctx, macA); ok {
		t.Fatal("empty database knows a host")
	}
	h := addHost(t, s, macA, "PC-01")
	if ok, err := s.IsHost(ctx, macA); !ok || err != nil {
		t.Fatalf("IsHost: %v %v", ok, err)
	}
	if _, err := s.AddHost(ctx, macA, "again"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate MAC: %v", err)
	}
	if err := s.RenameHost(ctx, h.ID, "PC-02"); err != nil {
		t.Fatalf("RenameHost: %v", err)
	}
	hosts, _ := s.Hosts(ctx)
	if hosts[0].Name != "PC-02" {
		t.Fatalf("name: %q", hosts[0].Name)
	}
	if err := s.DeleteHost(ctx, h.ID); err != nil {
		t.Fatalf("DeleteHost: %v", err)
	}
	if err := s.DeleteHost(ctx, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
	if err := s.RenameHost(ctx, h.ID, "x"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rename of a deleted host: %v", err)
	}
}

func TestDiscoveredBecomesHost(t *testing.T) {
	s := open(t)
	for range 2 {
		if err := s.Seen(ctx, macA); err != nil {
			t.Fatalf("Seen: %v", err)
		}
	}
	list, err := s.DiscoveredPCs(ctx)
	if err != nil || len(list) != 1 || list[0].MAC != macA.String() {
		t.Fatalf("discovered %+v, err %v", list, err)
	}

	addHost(t, s, macA, "PC-01")
	if list, _ := s.DiscoveredPCs(ctx); len(list) != 0 {
		t.Fatalf("still discovered: %+v", list)
	}
	if hosts, _ := s.Hosts(ctx); hosts[0].LastSeen == nil {
		t.Fatal("last seen was lost when the PC was registered")
	}
	if err := s.Seen(ctx, macA); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if list, _ := s.DiscoveredPCs(ctx); len(list) != 0 {
		t.Fatalf("a registered PC went back to discovered: %+v", list)
	}
	hosts, _ := s.Hosts(ctx)
	if hosts[0].LastSeen == nil {
		t.Fatal("last seen not recorded")
	}
}

func TestForgetDiscovered(t *testing.T) {
	s := open(t)
	if err := s.Seen(ctx, macA); err != nil {
		t.Fatalf("Seen: %v", err)
	}
	if err := s.ForgetDiscovered(ctx, macA); err != nil {
		t.Fatalf("ForgetDiscovered: %v", err)
	}
	if err := s.ForgetDiscovered(ctx, macA); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second forget: %v", err)
	}
}

func TestTaskLifecycle(t *testing.T) {
	s := open(t)
	h := addHost(t, s, macA, "PC-01")
	addHost(t, s, macB, "PC-02")

	task, err := s.CreateTask(ctx, h.ID, TaskAgent)
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.Status != StatusQueued {
		t.Fatalf("status: %q", task.Status)
	}
	if _, err := s.CreateTask(ctx, h.ID, TaskAgent); !errors.Is(err, ErrTaskExists) {
		t.Fatalf("second task: %v", err)
	}
	if ok, _ := s.HasTask(ctx, macA); !ok {
		t.Fatal("HasTask false for a queued task")
	}
	if ok, _ := s.HasTask(ctx, macB); ok {
		t.Fatal("task leaked to another host")
	}

	if err := s.MarkStarted(ctx, macA); err != nil {
		t.Fatalf("MarkStarted: %v", err)
	}
	hosts, _ := s.Hosts(ctx)
	if hosts[0].Task == nil || hosts[0].Task.Status != StatusStarted {
		t.Fatalf("task: %+v", hosts[0].Task)
	}
	if hosts[1].Task != nil {
		t.Fatalf("other host has a task: %+v", hosts[1].Task)
	}

	if err := s.CancelTask(ctx, h.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if ok, _ := s.HasTask(ctx, macA); ok {
		t.Fatal("cancelled task is still active")
	}
	if err := s.CancelTask(ctx, h.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second cancel: %v", err)
	}
	if _, err := s.CreateTask(ctx, h.ID, TaskAgent); err != nil {
		t.Fatalf("new task after cancel: %v", err)
	}
}

func TestCreateTaskForMissingHost(t *testing.T) {
	if _, err := open(t).CreateTask(ctx, 42, TaskAgent); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteHostRemovesTasks(t *testing.T) {
	s := open(t)
	h := addHost(t, s, macA, "PC-01")
	if _, err := s.CreateTask(ctx, h.ID, TaskAgent); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.DeleteHost(ctx, h.ID); err != nil {
		t.Fatalf("DeleteHost: %v", err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d tasks left, err %v", n, err)
	}
}
