package imaging

import (
	"time"

	"fogs/proto"
)

// Kinds of server work in the metrics of a task.
const (
	statOpen       = "открытие образа"
	statList       = "список блоков"
	statReadImage  = "чтение блоков образа с диска"
	statSend       = "отправка блоков в сеть"
	statCheck      = "проверка хэшей"
	statReceive    = "приём блоков из сети"
	statWriteImage = "запись блоков в файл образа"
	statFinish     = "сброс и проверка готового файла"
)

// taskStats is what the server did for one task: time by kind of work and amounts.
// The agent measures the same requests from its side, network included; the
// difference between the two is the network and the waiting.
type taskStats struct {
	work    []proto.MetricTime // nanoseconds until ServerStats turns them into ms
	amounts []proto.MetricAmount
}

// note adds d to a kind of work of a task.
func (m *Manager) note(task int64, kind string, d time.Duration) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	s := m.statsOf(task)
	for i := range s.work {
		if s.work[i].Name == kind {
			s.work[i].Ms += int64(d)
			return
		}
	}
	s.work = append(s.work, proto.MetricTime{Name: kind, Ms: int64(d)})
}

// count adds n to an amount of a task.
func (m *Manager) count(task int64, name string, n uint64, bytes bool) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	s := m.statsOf(task)
	for i := range s.amounts {
		if s.amounts[i].Name == name {
			s.amounts[i].Value += n
			return
		}
	}
	s.amounts = append(s.amounts, proto.MetricAmount{Name: name, Value: n, Bytes: bytes})
}

func (m *Manager) statsOf(task int64) *taskStats {
	s := m.stats[task]
	if s == nil {
		s = &taskStats{}
		m.stats[task] = s
	}
	return s
}

// ServerStats returns what the server did for a task so far, or nil. The stats
// outlive the clone or deploy, so the result of the agent still finds them; Abort
// forgets them.
func (m *Manager) ServerStats(task int64) ([]proto.MetricTime, []proto.MetricAmount) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	s := m.stats[task]
	if s == nil {
		return nil, nil
	}
	work := make([]proto.MetricTime, len(s.work))
	for i, w := range s.work {
		work[i] = proto.MetricTime{Name: w.Name, Ms: time.Duration(w.Ms).Milliseconds()}
	}
	return work, append([]proto.MetricAmount(nil), s.amounts...)
}

func (m *Manager) forgetStats(task int64) {
	m.statsMu.Lock()
	defer m.statsMu.Unlock()
	delete(m.stats, task)
}
