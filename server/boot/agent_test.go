package boot

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"fogs/proto"
	"fogs/server/imaging"
	"fogs/server/store"
)

var testMAC = net.HardwareAddr{0x04, 0x7c, 0x16, 0xe7, 0xd2, 0x1e}

func agentServer(t *testing.T) (http.Handler, *store.Store, store.Host) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "fogs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	host, err := s.AddHost(context.Background(), testMAC, "PC-12")
	if err != nil {
		t.Fatalf("AddHost: %v", err)
	}
	no := func(net.HardwareAddr) bool { return false }
	images := imaging.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := New(Config{
		HTTPAddr: "192.168.88.105:8080",
		Known:    no,
		HasTask:  no,
		Started:  func(net.HardwareAddr) {},
		Files:    fstest.MapFS{},
		Agent:    s,
		Clone:    images,
		Deploy:   images,
		Log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	return h, s, host
}

func call(h http.Handler, method, url string, body any) *httptest.ResponseRecorder {
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = strings.NewReader(string(b))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, url, r))
	return w
}

func TestAgentConfig(t *testing.T) {
	h, _, _ := agentServer(t)
	w := call(h, http.MethodGet, "/agent/04-7c-16-e7-d2-1e/fogs-agent.json", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var got proto.AgentConfig
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := proto.AgentConfig{Server: "http://192.168.88.105:8080", MAC: "04:7c:16:e7:d2:1e", Host: "PC-12"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	if w := call(h, http.MethodGet, "/agent/aa-bb-cc-00-00-09/fogs-agent.json", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown PC: %d", w.Code)
	}
}

func TestAgentTaskFlow(t *testing.T) {
	h, s, host := agentServer(t)
	ctx := context.Background()
	const taskURL = "/agent/task?mac=04:7c:16:e7:d2:1e"

	if w := call(h, http.MethodGet, taskURL, nil); w.Code != http.StatusNoContent {
		t.Fatalf("no task: %d", w.Code)
	}
	created, err := s.CreateTask(ctx, host.ID, store.TaskAgent, "")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w := call(h, http.MethodGet, taskURL, nil)
	var task proto.AgentTask
	if err := json.Unmarshal(w.Body.Bytes(), &task); err != nil || task.ID != created.ID || task.Kind != proto.TaskKindCheck {
		t.Fatalf("task %+v, err %v, body %s", task, err, w.Body)
	}

	base := "/agent/tasks/" + strconv.FormatInt(task.ID, 10)
	mac := testMAC.String()
	if w := call(h, http.MethodPost, base+"/progress", proto.AgentProgress{MAC: mac, Percent: 50, Stage: "Сервер"}); w.Code != http.StatusNoContent {
		t.Fatalf("progress: %d %s", w.Code, w.Body)
	}
	hosts, _ := s.Hosts(ctx)
	if tk := hosts[0].Task; tk == nil || tk.Percent != 50 || tk.Status != store.StatusRunning {
		t.Fatalf("task: %+v", tk)
	}
	if w := call(h, http.MethodPost, base+"/finish", proto.AgentResult{MAC: mac, OK: true, Message: "ок"}); w.Code != http.StatusNoContent {
		t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
	if w := call(h, http.MethodPost, base+"/progress", proto.AgentProgress{MAC: mac, Percent: 60}); w.Code != http.StatusGone {
		t.Fatalf("report after finish: %d", w.Code)
	}
	if w := call(h, http.MethodGet, taskURL, nil); w.Code != http.StatusNoContent {
		t.Fatalf("task after finish: %d", w.Code)
	}
}

func TestCancelTellsAgentToStop(t *testing.T) {
	h, s, host := agentServer(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, host.ID, store.TaskAgent, "")
	if err := s.CancelTask(ctx, host.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	url := "/agent/tasks/" + strconv.FormatInt(task.ID, 10) + "/progress"
	if w := call(h, http.MethodPost, url, proto.AgentProgress{MAC: testMAC.String(), Percent: 1}); w.Code != http.StatusGone {
		t.Fatalf("status %d", w.Code)
	}
}

func TestAgentRejectsBadReports(t *testing.T) {
	h, s, host := agentServer(t)
	task, _ := s.CreateTask(context.Background(), host.ID, store.TaskAgent, "")
	base := "/agent/tasks/" + strconv.FormatInt(task.ID, 10)
	mac := testMAC.String()

	tests := []struct {
		name string
		url  string
		body any
		want int
	}{
		{"percent above 100", base + "/progress", proto.AgentProgress{MAC: mac, Percent: 101}, http.StatusBadRequest},
		{"negative percent", base + "/progress", proto.AgentProgress{MAC: mac, Percent: -1}, http.StatusBadRequest},
		{"bad mac", base + "/progress", proto.AgentProgress{MAC: "nope"}, http.StatusBadRequest},
		{"bad id", "/agent/tasks/abc/finish", proto.AgentResult{MAC: mac}, http.StatusBadRequest},
		{"another PC", base + "/finish", proto.AgentResult{MAC: "aa:bb:cc:00:00:09", OK: true}, http.StatusGone},
		{"huge report", base + "/progress", proto.AgentProgress{MAC: mac, Message: strings.Repeat("x", maxReportBody+1)}, http.StatusBadRequest},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if w := call(h, http.MethodPost, tc.url, tc.body); w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body)
			}
		})
	}
}

func TestLongTextIsClipped(t *testing.T) {
	h, s, host := agentServer(t)
	ctx := context.Background()
	task, _ := s.CreateTask(ctx, host.ID, store.TaskAgent, "")
	url := "/agent/tasks/" + strconv.FormatInt(task.ID, 10) + "/progress"
	if w := call(h, http.MethodPost, url, proto.AgentProgress{MAC: testMAC.String(), Message: strings.Repeat("я", 600)}); w.Code != http.StatusNoContent {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	hosts, _ := s.Hosts(ctx)
	if n := len([]rune(hosts[0].Task.Message)); n != maxReportText {
		t.Fatalf("message is %d characters", n)
	}
}

// TestForeignRequestLeavesATaskAlone: a request with somebody else's MAC is refused
// and must not drop the clone of the PC that owns the task.
func TestForeignRequestLeavesATaskAlone(t *testing.T) {
	h, s, host := agentServer(t)
	ctx := context.Background()
	if err := s.SetSetting(ctx, store.SettingImageDir, t.TempDir()); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}
	task, err := s.CreateTask(ctx, host.ID, store.TaskClone, "win11")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	base := "/agent/tasks/" + strconv.FormatInt(task.ID, 10)
	own := "?mac=" + testMAC.String()
	if w := call(h, http.MethodPost, base+"/clone/start"+own, nil); w.Code != http.StatusOK {
		t.Fatalf("start: %d %s", w.Code, w.Body)
	}
	const stranger = "?mac=02:00:00:00:00:01"
	for _, step := range []string{"/clone/abort", "/clone/check", "/deploy/abort"} {
		if w := call(h, http.MethodPost, base+step+stranger, nil); w.Code != http.StatusGone {
			t.Fatalf("%s from a stranger: %d", step, w.Code)
		}
	}
	if w := call(h, http.MethodPost, base+"/finish", proto.AgentResult{MAC: "02:00:00:00:00:01", OK: false}); w.Code != http.StatusGone {
		t.Fatalf("finish from a stranger: %d", w.Code)
	}
	if w := call(h, http.MethodPost, base+"/clone/check"+own, nil); w.Code != http.StatusOK {
		t.Fatalf("the clone was dropped: %d %s", w.Code, w.Body)
	}

	// Once the task is over, what it left is dropped, whoever notices.
	if err := s.CancelTask(ctx, host.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if w := call(h, http.MethodPost, base+"/clone/check"+stranger, nil); w.Code != http.StatusGone {
		t.Fatalf("check after cancel: %d", w.Code)
	}
	task2, err := s.CreateTask(ctx, host.ID, store.TaskClone, "win11")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	url := "/agent/tasks/" + strconv.FormatInt(task2.ID, 10) + "/clone/start" + own
	if w := call(h, http.MethodPost, url, nil); w.Code != http.StatusOK {
		t.Fatalf("the old clone still holds the image: %d %s", w.Code, w.Body)
	}
	abort := "/agent/tasks/" + strconv.FormatInt(task2.ID, 10) + "/clone/abort" + own
	if w := call(h, http.MethodPost, abort, nil); w.Code != http.StatusNoContent {
		t.Fatalf("abort: %d", w.Code)
	}
}

// TestSavedTableComesBackWithTheNextTask: the table saved before a shrink reaches
// the agent even after the task that saved it was cancelled.
func TestSavedTableComesBackWithTheNextTask(t *testing.T) {
	h, s, host := agentServer(t)
	ctx := context.Background()
	url := "/agent/table?mac=" + testMAC.String()
	r := httptest.NewRequest(http.MethodPost, url, strings.NewReader("table"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("save: %d", w.Code)
	}
	if hosts, _ := s.Hosts(ctx); !hosts[0].Shrunk {
		t.Fatalf("host: %+v", hosts[0])
	}
	if _, err := s.CreateTask(ctx, host.ID, store.TaskAgent, ""); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	w = call(h, http.MethodGet, "/agent/task?mac="+testMAC.String(), nil)
	var got proto.AgentTask
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || string(got.Table) != "table" {
		t.Fatalf("task: %+v %v", got, err)
	}
	if w := call(h, http.MethodPost, url, nil); w.Code != http.StatusNoContent {
		t.Fatalf("clear: %d", w.Code)
	}
	if hosts, _ := s.Hosts(ctx); hosts[0].Shrunk {
		t.Fatalf("host: %+v", hosts[0])
	}
	if w := call(h, http.MethodPost, "/agent/table?mac=02:00:00:00:00:01", nil); w.Code != http.StatusNotFound {
		t.Fatalf("unknown PC: %d", w.Code)
	}
}

// TestMetricsReachTheLog: a line of the log is enough to see where a task spent
// its time, the work inside the stages and the amounts included.
func TestMetricsReachTheLog(t *testing.T) {
	_, s, host := agentServer(t)
	task, err := s.CreateTask(context.Background(), host.ID, store.TaskDeploy, "win11")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	var log strings.Builder
	c := Config{Agent: s, Log: slog.New(slog.NewTextHandler(&log, nil))}
	m := &proto.TaskMetrics{
		TotalMs: 61000,
		Steps:   []proto.MetricTime{{Name: "Загрузка и запись блоков", Ms: 60000}},
		Work:    []proto.MetricTime{{Name: "запись ждёт загрузку", Ms: 30000}},
		Amounts: []proto.MetricAmount{{Name: "записано на диск", Value: 3 << 20, Bytes: true}, {Name: "пачек", Value: 7}},
	}
	saveMetrics(context.Background(), c, testMAC, task.ID, m)
	for _, want := range []string{"total=1m1s", "steps.Загрузка и запись блоков\"=1m0s", "work.запись ждёт загрузку\"=30s",
		"amounts.записано на диск, MiB\"=3", "amounts.пачек=7"} {
		if !strings.Contains(log.String(), want) {
			t.Fatalf("no %q in %s", want, log.String())
		}
	}
	if data, err := s.TaskMetrics(context.Background(), task.ID); err != nil || !strings.Contains(data, "пачек") {
		t.Fatalf("stored %q, err %v", data, err)
	}
}

func TestClipDetails(t *testing.T) {
	if got := clipDetails("короткий"); got != "короткий" {
		t.Fatalf("short: %q", got)
	}
	long := strings.Repeat("я", maxReportDetails) // two bytes each
	got := clipDetails(long)
	if len(got) > maxReportDetails || !utf8.ValidString(got) || len(got) < maxReportDetails-1 {
		t.Fatalf("%d bytes, valid %v", len(got), utf8.ValidString(got))
	}
	odd := "x" + long // the cut falls in the middle of a letter
	if got := clipDetails(odd); !utf8.ValidString(got) || len(got) != maxReportDetails-1 {
		t.Fatalf("odd: %d bytes, valid %v", len(got), utf8.ValidString(got))
	}
}

// TestCancelledTaskKeepsItsMetrics: the report of a task cancelled meanwhile is
// refused, and its metrics are still kept, while another PC cannot write them.
func TestCancelledTaskKeepsItsMetrics(t *testing.T) {
	h, s, host := agentServer(t)
	ctx := context.Background()
	task, err := s.CreateTask(ctx, host.ID, store.TaskClone, "win11")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := s.CancelTask(ctx, host.ID); err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	url := "/agent/tasks/" + strconv.FormatInt(task.ID, 10) + "/progress"
	m := &proto.TaskMetrics{TotalMs: 1500, Steps: []proto.MetricTime{{Name: "Сжатие тома", Ms: 1500}}}
	other := proto.AgentProgress{MAC: "aa:bb:cc:00:00:09", Stage: proto.StageFailed, Message: "x", Metrics: m}
	if w := call(h, http.MethodPost, url, other); w.Code != http.StatusGone {
		t.Fatalf("another PC: %d", w.Code)
	}
	if data, _ := s.TaskMetrics(ctx, task.ID); data != "" {
		t.Fatalf("another PC wrote %q", data)
	}
	own := proto.AgentProgress{MAC: testMAC.String(), Stage: proto.StageFailed, Message: "x", Metrics: m}
	if w := call(h, http.MethodPost, url, own); w.Code != http.StatusGone {
		t.Fatalf("cancelled task: %d", w.Code)
	}
	if data, _ := s.TaskMetrics(ctx, task.ID); !strings.Contains(data, "Сжатие тома") {
		t.Fatalf("metrics of the cancelled task: %q", data)
	}
}

// TestBootTimeReachesTheMetrics: the server measures from the boot script to the
// first request of the agent, and puts it into the metrics of the task.
func TestBootTimeReachesTheMetrics(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "fogs.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	host, _ := s.AddHost(ctx, testMAC, "PC-12")
	task, err := s.CreateTask(ctx, host.ID, store.TaskClone, "win11")
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	yes := func(net.HardwareAddr) bool { return true }
	images := imaging.New(s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	h := New(Config{HTTPAddr: "192.168.88.105:8080", Known: yes, HasTask: yes, Started: func(net.HardwareAddr) {},
		Files: fstest.MapFS{}, Agent: s, Clone: images, Deploy: images, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})

	if w := call(h, http.MethodGet, "/boot.ipxe?mac="+testMAC.String(), nil); w.Code != http.StatusOK {
		t.Fatalf("boot.ipxe: %d", w.Code)
	}
	time.Sleep(20 * time.Millisecond)
	if w := call(h, http.MethodGet, "/agent/task?mac="+testMAC.String(), nil); w.Code != http.StatusOK {
		t.Fatalf("task: %d", w.Code)
	}
	url := "/agent/tasks/" + strconv.FormatInt(task.ID, 10) + "/finish"
	res := proto.AgentResult{MAC: testMAC.String(), OK: true, Message: "ok", Metrics: &proto.TaskMetrics{TotalMs: 1}}
	if w := call(h, http.MethodPost, url, res); w.Code != http.StatusNoContent {
		t.Fatalf("finish: %d %s", w.Code, w.Body)
	}
	data, _ := s.TaskMetrics(ctx, task.ID)
	var m proto.TaskMetrics
	if err := json.Unmarshal([]byte(data), &m); err != nil || m.Server == nil || m.Server.BootMs < 20 {
		t.Fatalf("metrics %q, err %v", data, err)
	}
}
