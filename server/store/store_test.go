package store

import (
	"context"
	"database/sql"
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
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = old.Exec(migrations[0] + `
		INSERT INTO hosts(id, mac, name, created_at) VALUES(1, 'aa:bb:cc:00:00:01', 'PC-01', 1);
		INSERT INTO tasks(host_id, kind, status, created_at) VALUES(1, 'agent', 'queued', 1);
		PRAGMA user_version = 1;`)
	if err != nil {
		t.Fatalf("old schema: %v", err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	defer s.Close()
	hosts, err := s.Hosts(ctx)
	if err != nil || len(hosts) != 1 || hosts[0].Task == nil {
		t.Fatalf("hosts %+v, err %v", hosts, err)
	}
	if err := s.SetSetting(ctx, SettingImageDir, "x"); err != nil {
		t.Fatalf("SetSetting after upgrade: %v", err)
	}
	if err := s.ReportProgress(ctx, macA, hosts[0].Task.ID, 10, "", ""); err != nil {
		t.Fatalf("ReportProgress after upgrade: %v", err)
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

	task, err := s.CreateTask(ctx, h.ID, TaskAgent, "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if task.Status != StatusQueued {
		t.Fatalf("status: %q", task.Status)
	}
	if _, err := s.CreateTask(ctx, h.ID, TaskAgent, ""); !errors.Is(err, ErrTaskExists) {
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
	if _, err := s.CreateTask(ctx, h.ID, TaskAgent, ""); err != nil {
		t.Fatalf("new task after cancel: %v", err)
	}
}

func TestAgentReportsTask(t *testing.T) {
	s := open(t)
	h := addHost(t, s, macA, "PC-01")
	addHost(t, s, macB, "PC-02")
	task, err := s.CreateTask(ctx, h.ID, TaskAgent, "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	got, err := s.ActiveTask(ctx, macA)
	if err != nil || got == nil || got.ID != task.ID || got.Kind != TaskAgent {
		t.Fatalf("ActiveTask: %+v, err %v", got, err)
	}
	if got, err := s.ActiveTask(ctx, macB); got != nil || err != nil {
		t.Fatalf("task for another PC: %+v, err %v", got, err)
	}
	if name, err := s.HostName(ctx, macA); name != "PC-01" || err != nil {
		t.Fatalf("HostName: %q, err %v", name, err)
	}

	if err := s.ReportProgress(ctx, macA, task.ID, 40, "Сеть", "ок"); err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	if err := s.ReportProgress(ctx, macB, task.ID, 99, "x", "y"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report from another PC: %v", err)
	}
	hosts, _ := s.Hosts(ctx)
	if tk := hosts[0].Task; tk == nil || tk.Status != StatusRunning || tk.Percent != 40 || tk.Stage != "Сеть" {
		t.Fatalf("task: %+v", tk)
	}

	if err := s.FinishTask(ctx, macB, task.ID, true, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("finish from another PC: %v", err)
	}
	if err := s.FinishTask(ctx, macA, task.ID, true, "связь есть"); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	if ok, _ := s.HasTask(ctx, macA); ok {
		t.Fatal("finished task is still active, the PC would boot WinPE again")
	}
	if err := s.ReportProgress(ctx, macA, task.ID, 50, "", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("report after finish: %v", err)
	}
	hosts, _ = s.Hosts(ctx)
	last := hosts[0].LastTask
	if hosts[0].Task != nil || last == nil || last.Status != StatusDone || last.Percent != 100 || last.Message != "связь есть" || last.FinishedAt == nil {
		t.Fatalf("host: %+v, last %+v", hosts[0], last)
	}
	if hosts[1].LastTask != nil {
		t.Fatalf("result leaked to another host: %+v", hosts[1].LastTask)
	}
}

func TestFailedTaskKeepsItsProgress(t *testing.T) {
	s := open(t)
	h := addHost(t, s, macA, "PC-01")
	task, _ := s.CreateTask(ctx, h.ID, TaskAgent, "")
	if err := s.ReportProgress(ctx, macA, task.ID, 47, "", ""); err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	if err := s.FinishTask(ctx, macA, task.ID, false, "обрыв"); err != nil {
		t.Fatalf("FinishTask: %v", err)
	}
	hosts, _ := s.Hosts(ctx)
	if last := hosts[0].LastTask; last == nil || last.Status != StatusFailed || last.Percent != 47 {
		t.Fatalf("last: %+v", last)
	}
}

func TestCancelledTaskIsNotAResult(t *testing.T) {
	s := open(t)
	h := addHost(t, s, macA, "PC-01")
	if _, err := s.CreateTask(ctx, h.ID, TaskAgent, ""); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.CancelTask(ctx, h.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if hosts, _ := s.Hosts(ctx); hosts[0].LastTask != nil {
		t.Fatalf("last: %+v", hosts[0].LastTask)
	}
}

func TestImageLifecycle(t *testing.T) {
	s := open(t)
	h := addHost(t, s, macA, "PC-01")

	if err := s.CreateImage(ctx, "win11", filepath.Join(t.TempDir(), "win11")); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := s.CreateImage(ctx, "win11", filepath.Join(t.TempDir(), "win11")); !errors.Is(err, ErrImageExists) {
		t.Fatalf("second create: %v", err)
	}
	if images, _ := s.Images(ctx); len(images) != 1 || len(images[0].Versions) != 0 {
		t.Fatalf("images: %+v", images)
	}

	if err := s.SetHostImage(ctx, h.ID, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing image: %v", err)
	}
	if err := s.SetHostImage(ctx, h.ID, "win11"); err != nil {
		t.Fatalf("SetHostImage: %v", err)
	}
	if name, _ := s.HostImage(ctx, h.ID); name != "win11" {
		t.Fatalf("HostImage: %q", name)
	}
	if hosts, _ := s.Hosts(ctx); hosts[0].Image != "win11" {
		t.Fatalf("host: %+v", hosts[0])
	}

	if err := s.AddVersion(ctx, "win11", Version{Number: 1, File: "v1.ldisk", Size: 1, SourceMAC: macA.String()}); err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if err := s.DeleteImage(ctx, "win11"); !errors.Is(err, ErrImageInUse) {
		t.Fatalf("delete with versions: %v", err)
	}

	if err := s.CreateImage(ctx, "empty", filepath.Join(t.TempDir(), "empty")); err != nil {
		t.Fatalf("CreateImage: %v", err)
	}
	if err := s.SetHostImage(ctx, h.ID, "empty"); err != nil {
		t.Fatalf("SetHostImage: %v", err)
	}
	if err := s.DeleteImage(ctx, "empty"); err != nil {
		t.Fatalf("DeleteImage: %v", err)
	}
	if name, err := s.HostImage(ctx, h.ID); name != "" || err != nil {
		t.Fatalf("link survived: %q %v", name, err)
	}
	if err := s.SetHostImage(ctx, h.ID, ""); err != nil {
		t.Fatalf("clearing: %v", err)
	}
	if _, err := s.HostImage(ctx, 42); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing host: %v", err)
	}
}

func TestCreateTaskForMissingHost(t *testing.T) {
	if _, err := open(t).CreateTask(ctx, 42, TaskAgent, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestDeleteHostRemovesTasks(t *testing.T) {
	s := open(t)
	h := addHost(t, s, macA, "PC-01")
	if _, err := s.CreateTask(ctx, h.ID, TaskAgent, ""); err != nil {
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

// TestOldImagesGetTheirPath: an image created before paths were stored stays where
// the image directory said it was on the day of the upgrade.
func TestOldImagesGetTheirPath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "db")
	s, err := Open(file)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.SetSetting(ctx, SettingImageDir, "D:\\images"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	if _, err := s.db.Exec("INSERT INTO images(name, created_at) VALUES('old', 1)"); err != nil {
		t.Fatalf("insert: %v", err)
	}
	s.Close()
	if s, err = Open(file); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()
	if err := s.SetSetting(ctx, SettingImageDir, "E:\\other"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	want := filepath.Join("D:\\images", "old")
	if got, err := s.ImagePath(ctx, "old"); got != want || err != nil {
		t.Fatalf("ImagePath = %q, %v", got, err)
	}
}
