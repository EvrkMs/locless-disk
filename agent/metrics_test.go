package main

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"fogs/proto"
	"fogs/server/store"
)

func TestRecorderCountsStepsWorkAndAmounts(t *testing.T) {
	clock := time.Unix(1000, 0)
	r := newRecorder(1)
	r.now = func() time.Time { return clock }
	r.started = clock

	r.Step("первый")
	clock = clock.Add(2 * time.Second)
	r.Step("второй")
	stop := r.Time("сеть")
	clock = clock.Add(3 * time.Second)
	stop()
	r.Add("сеть", time.Second)
	r.Bytes("байты", 10)
	r.Bytes("байты", 5)
	r.Count("штуки", 1)
	r.Step("первый") // a stage that comes again adds to its entry

	clock = clock.Add(time.Second)
	m := r.Result()
	if m.TotalMs != 6000 {
		t.Fatalf("total %d", m.TotalMs)
	}
	want := []proto.MetricTime{{Name: "первый", Ms: 3000}, {Name: "второй", Ms: 3000}}
	if !slices.Equal(m.Steps, want) {
		t.Fatalf("steps %+v", m.Steps)
	}
	if !slices.Equal(m.Work, []proto.MetricTime{{Name: "сеть", Ms: 4000}}) {
		t.Fatalf("work %+v", m.Work)
	}
	wantAmounts := []proto.MetricAmount{{Name: "байты", Value: 15, Bytes: true}, {Name: "штуки", Value: 1}}
	if !slices.Equal(m.Amounts, wantAmounts) {
		t.Fatalf("amounts %+v", m.Amounts)
	}
	if m.Cores < 1 || m.PeakMemory == 0 {
		t.Fatalf("resources %+v", m)
	}

	// Result changes nothing: the running stage is only counted up to now.
	clock = clock.Add(time.Second)
	if again := r.Result(); again.Steps[0].Ms != 4000 || len(again.Steps) != 2 {
		t.Fatalf("second result %+v", again.Steps)
	}

	r.Note("Диск", "первый")
	r.Note("Сжатие тома", "не сжат")
	r.Note("Диск", "второй") // a note of the same name replaces the first
	want2 := []proto.MetricNote{{Name: "Диск", Value: "второй"}, {Name: "Сжатие тома", Value: "не сжат"}}
	if got := r.Result().Notes; !slices.Equal(got, want2) {
		t.Fatalf("notes %+v", got)
	}
}

// TestRecorderKeepsShortWork: a block is hashed in well under a millisecond, and
// thousands of those must add up instead of each counting as nothing.
func TestRecorderKeepsShortWork(t *testing.T) {
	clock := time.Unix(1000, 0)
	r := newRecorder(1)
	r.now = func() time.Time { return clock }
	r.started = clock
	r.Step("этап")
	for range 100 {
		stop := r.Time("хэш")
		clock = clock.Add(700 * time.Microsecond)
		stop()
		r.Step("этап") // a stage started again many times
	}
	m := r.Result()
	if m.Work[0].Ms != 70 || m.Steps[0].Ms != 70 || m.TotalMs != 70 {
		t.Fatalf("work %+v, steps %+v, total %d", m.Work, m.Steps, m.TotalMs)
	}
}

func TestNilRecorderRecordsNothing(t *testing.T) {
	var r *recorder
	r.Step("x")
	r.Time("y")()
	r.Count("z", 1)
	r.Sample()
	if r.Result() != nil {
		t.Fatal("a nil recorder has metrics")
	}
}

// lastMetrics returns the metrics the server keeps for the newest task.
func lastMetrics(t *testing.T, s *store.Store) proto.TaskMetrics {
	t.Helper()
	tasks, err := s.RecentTasks(context.Background(), 1)
	if err != nil || len(tasks) != 1 || !tasks[0].HasMetrics {
		t.Fatalf("tasks %+v, err %v", tasks, err)
	}
	data, err := s.TaskMetrics(context.Background(), tasks[0].ID)
	if err != nil {
		t.Fatalf("TaskMetrics: %v", err)
	}
	var m proto.TaskMetrics
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("metrics %q: %v", data, err)
	}
	return m
}

func amount(m proto.TaskMetrics, name string) uint64 {
	for _, a := range m.Amounts {
		if a.Name == name {
			return a.Value
		}
	}
	return 0
}

func note(m proto.TaskMetrics, name string) string {
	for _, n := range m.Notes {
		if n.Name == name {
			return n.Value
		}
	}
	return ""
}

func names(list []proto.MetricTime) []string {
	var out []string
	for _, x := range list {
		out = append(out, x.Name)
	}
	return out
}

// checkTimes checks that the stages add up to no more than the whole task.
func checkTimes(t *testing.T, m proto.TaskMetrics) {
	t.Helper()
	var sum int64
	for _, s := range m.Steps {
		sum += s.Ms
	}
	if sum > m.TotalMs {
		t.Fatalf("stages take %d ms of %d", sum, m.TotalMs)
	}
}

// checkServer checks the parts every result carries besides the work of the agent:
// the layout version, the try, and the side of the server.
func checkServer(t *testing.T, m proto.TaskMetrics, kinds ...string) {
	t.Helper()
	if m.V != proto.MetricsVersion || m.Attempt != 1 {
		t.Fatalf("version %d, attempt %d", m.V, m.Attempt)
	}
	if m.Server == nil {
		t.Fatal("no metrics of the server")
	}
	for _, kind := range kinds {
		if !slices.Contains(names(m.Server.Work), kind) {
			t.Fatalf("no %q in %v", kind, names(m.Server.Work))
		}
	}
}

func TestCloneSendsItsMetrics(t *testing.T) {
	e := newCloneEnv(t)
	blocks := map[uint32][]byte{}
	for pos := range uint32(cloneBatch + 3) {
		blocks[pos] = fill(byte(pos+1), testBlock)
	}
	blocks[cloneBatch+3] = make([]byte, testBlock) // used, and all zeros
	e.run(t, &fakeDisk{blocks: blocks, size: 100 * testBlock})

	m := lastMetrics(t, e.s)
	checkTimes(t, m)
	checkServer(t, m, "открытие образа", "проверка хэшей", "приём блоков из сети", "запись блоков в файл образа", "сброс и проверка готового файла")
	if note(m, "Диск") != "тестовый диск" || note(m, "Сжатие тома") != "том сжат: 100 МБ → 40 МБ" {
		t.Fatalf("notes %+v", m.Notes)
	}
	wantSteps := []string{"Открытие диска", "Ожидание очереди на образ", "Сжатие тома", "Чтение диска и отправка",
		"Возврат размера тома", "Проверка и сохранение на сервере"}
	if got := names(m.Steps); !slices.Equal(got, wantSteps) {
		t.Fatalf("steps %v", got)
	}
	for _, kind := range []string{workDiskRead, workHash, workCheck, workCompress, workBuild, workUpload} {
		if !slices.Contains(names(m.Work), kind) {
			t.Fatalf("no %q in %v", kind, names(m.Work))
		}
	}
	if got := amount(m, "прочитано с диска"); got != uint64(len(blocks))*testBlock {
		t.Fatalf("read %d bytes", got)
	}
	if amount(m, "блоков с данными") != cloneBatch+3 || amount(m, "нулевых блоков") != 1 ||
		amount(m, "новых блоков отправлено") != cloneBatch+3 || amount(m, "пачек") != 2 {
		t.Fatalf("amounts %+v", m.Amounts)
	}
	if amount(m, "отправлено по сети") == 0 {
		t.Fatalf("amounts %+v", m.Amounts)
	}

	// The same disk again: everything is already on the server.
	e.run(t, &fakeDisk{blocks: blocks, size: 100 * testBlock})
	m = lastMetrics(t, e.s)
	if amount(m, "блоков уже было на сервере") != cloneBatch+3 || amount(m, "новых блоков отправлено") != 0 {
		t.Fatalf("second clone %+v", m.Amounts)
	}
}

func TestDeploySendsItsMetrics(t *testing.T) {
	e := newCloneEnv(t)
	blocks := map[uint32][]byte{}
	for pos := range uint32(cloneBatch + 3) {
		blocks[pos] = fill(byte(pos+1), testBlock)
	}
	blocks[cloneBatch+3] = make([]byte, testBlock)
	e.run(t, &fakeDisk{blocks: blocks, size: 100 * testBlock})

	if _, err := e.deploy(t, &fakeTarget{blocks: map[uint64][]byte{}}); err != nil {
		t.Fatalf("attempt: %v", err)
	}
	m := lastMetrics(t, e.s)
	checkTimes(t, m)
	checkServer(t, m, "открытие образа", "список блоков", "чтение блоков образа с диска", "отправка блоков в сеть")
	wantSteps := []string{"Открытие диска", "Ожидание очереди на образ", "Отсчёт перед стиранием", "Таблица разделов",
		"Список блоков", "Запись нулевых блоков", "Загрузка и запись блоков", "Сброс записи на диск",
		"Расширение тома", "Загрузочная запись", "Имя ПК", "Отчёт серверу"}
	if got := names(m.Steps); !slices.Equal(got, wantSteps) {
		t.Fatalf("steps %v", got)
	}
	if note(m, "Диск") != "тестовый диск" {
		t.Fatalf("notes %+v", m.Notes)
	}
	for _, kind := range []string{workFetch, workDecode, workWrite, workFetchWaits, workUnpackWaits, workWriteWaits} {
		if !slices.Contains(names(m.Work), kind) {
			t.Fatalf("no %q in %v", kind, names(m.Work))
		}
	}
	if got := amount(m, "записано на диск"); got != uint64(len(blocks))*testBlock {
		t.Fatalf("wrote %d bytes", got)
	}
	if amount(m, "блоков с данными") != cloneBatch+3 || amount(m, "нулевых блоков") != 1 ||
		amount(m, "пачек") != 2 || amount(m, "загружено по сети") == 0 {
		t.Fatalf("amounts %+v", m.Amounts)
	}
}

// TestUnshrunkVolumeSaysWhy: a clone that leaves the volume as it is tells why, so
// a stage of seconds with nothing to give back afterwards is not a mystery.
func TestUnshrunkVolumeSaysWhy(t *testing.T) {
	e := newCloneEnv(t)
	blocks := map[uint32][]byte{0: fill(1, testBlock)}
	e.run(t, &fakeDisk{blocks: blocks, size: 10 * testBlock, noShrink: true})
	m := lastMetrics(t, e.s)
	if got := note(m, "Сжатие тома"); got != "не сжат: сжатие: том уже почти полон, сжимать нечего" {
		t.Fatalf("note %q", got)
	}
}
