package main

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"fogs/agent/ui"
	"fogs/proto"
	"fogs/recovery"
)

// How long the agent keeps trying before it shows an error.
const (
	networkTries = 45 // WinPE can take a while to get an address
	serverTries  = 30
	finishTries  = 20
	reportFails  = 10 // reports in a row that may fail during a task
	retryDelay   = 2 * time.Second
	rebootDelay  = 10   // seconds on screen before the reboot
	maxDetails   = 4000 // bytes of a stack sent to the server, which takes small reports
)

// checkSteps is how long the link check runs, one report per step.
const checkSteps = 20

// screen holds what the window shows. The worker writes, the window reads.
type screen struct {
	mu    sync.Mutex
	state ui.State
}

func (s *screen) update(f func(*ui.State)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(&s.state)
}

// snapshot returns a copy that is safe to draw while the worker goes on.
func (s *screen) snapshot() ui.State {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state
	st.Stages = append([]ui.Stage(nil), st.Stages...)
	st.Log = append([]string(nil), st.Log...)
	return st
}

func (s *screen) log(line string) {
	s.update(func(st *ui.State) { st.Log = append(st.Log, line) })
}

// worker carries out what the server asks for.
type worker struct {
	cfg        proto.AgentConfig
	client     *client
	screen     *screen
	localIP    func() string          // the address of this PC, "" if there is none
	sleep      func(time.Duration)    // replaced in tests
	reboot     func()                 // ends the session
	retry      chan struct{}          // the operator pressed R
	openDisk   func() (source, error) // the disk to clone
	openTarget func() (target, error) // the disk to overwrite
	abort      chan struct{}          // the operator pressed Esc before a deploy
	names      []string               // stages of the current task
	taskID     int64                  // the task being worked on, 0 before the server named one
	failed     atomic.Bool            // the error screen is up, so a reboot cannot break anything
	rec        *recorder              // metrics of the clone or deploy running now, nil otherwise
	attempts   int                    // tries of the task since this boot, for the metrics
}

// Stages of the link check, in order.
var checkStages = []string{"Сеть", "Сервер", "Проверка связи", "Завершение"}

const (
	stageNetwork = iota
	stageServer
	stageCheck
	stageFinish
)

// run works until the PC should reboot. After an error it waits for the operator.
func (w *worker) run() {
	for {
		// A bug must end on the error screen too, with the reboot key working and the
		// server told, and not in a WinPE that closes with the PC half done.
		err := recovery.Run(w.attempt)
		if err == nil {
			w.countdown()
			w.reboot()
			return
		}
		w.showError(err)
		w.failed.Store(true) // before the report, which can take seconds on a dead network
		w.report(err)
		<-w.retry
		w.failed.Store(false)
	}
}

// attempt does everything from waiting for the network to reporting the result.
func (w *worker) attempt() error {
	w.rec = nil
	w.attempts++
	w.names = checkStages
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseIdle, -1
		st.Center, st.Caption = "Агент", "подключение"
		st.Title, st.Detail, st.Hint = "Подключение", "Сервер: "+w.cfg.Server, ""
		st.Stages = stages(w.names, stageNetwork, ui.StageActive)
	})

	ip, err := w.waitNetwork()
	if err != nil {
		return err
	}
	w.screen.log("сеть: адрес " + ip)
	w.setStage(stageServer)

	task, err := w.fetchTask()
	if err != nil {
		return err
	}
	if task == nil {
		w.screen.log("сервер: задачи нет")
		w.showResult("Задачи нет", "Компьютер загрузится с диска")
		return nil
	}
	w.screen.log(fmt.Sprintf("сервер: задача %d", task.ID))
	w.taskID = task.ID

	if task.Kind == proto.TaskKindDeploy {
		return w.runDeploy(task)
	}
	if task.Kind == proto.TaskKindFormat {
		return w.runFormat(task)
	}
	if len(task.Table) > 0 {
		if err := w.repair(task.Table); err != nil {
			return err
		}
	}
	if task.Kind == proto.TaskKindClone {
		return w.runClone(task)
	}
	if task.Kind != proto.TaskKindCheck {
		message := "неизвестный тип задачи: " + task.Kind
		if err := w.finish(task.ID, false, message); err != nil {
			return err
		}
		return errors.New(message)
	}
	return w.runCheck(task.ID, ip)
}

// waitNetwork waits until WinPE has an address.
func (w *worker) waitNetwork() (string, error) {
	for range networkTries {
		if ip := w.localIP(); ip != "" {
			return ip, nil
		}
		w.sleep(retryDelay)
	}
	return "", errors.New("нет адреса в сети: возможно, в WinPE нет драйвера сетевой карты")
}

// fetchTask asks the server for a task, retrying while it is unreachable.
func (w *worker) fetchTask() (*proto.AgentTask, error) {
	var last error
	for range serverTries {
		task, err := w.client.task()
		if err == nil {
			return task, nil
		}
		last = err
		w.sleep(retryDelay)
	}
	return nil, fmt.Errorf("сервер не отвечает: %w", last)
}

// runCheck exercises the whole link: many small reports, then the result.
func (w *worker) runCheck(id int64, ip string) error {
	w.setStage(stageCheck)
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent = ui.PhaseDeploy, 0
		st.Title, st.Detail = "Проверка связи", "Диск не меняется"
	})

	fails := 0
	for step := 1; step <= checkSteps; step++ {
		percent := step * 100 / checkSteps
		w.screen.update(func(st *ui.State) {
			st.Percent, st.Center, st.Caption = float64(percent), fmt.Sprintf("%d%%", percent), "связь с сервером"
		})
		switch err := w.client.progress(id, percent, checkStages[stageCheck], "адрес "+ip); {
		case errors.Is(err, errTaskGone):
			w.screen.log("сервер: задача отменена")
			w.showResult("Задача отменена", "Компьютер загрузится с диска")
			return nil
		case err != nil:
			fails++
			w.screen.log("отчёт не доставлен: " + err.Error())
			if fails >= reportFails {
				return fmt.Errorf("связь с сервером потеряна: %w", err)
			}
		default:
			fails = 0
		}
		w.sleep(time.Second)
	}

	w.setStage(stageFinish)
	if err := w.finish(id, true, "связь есть, адрес "+ip); err != nil {
		return err
	}
	w.screen.log("сервер: задача завершена")
	w.showResult("Готово", "Связь с сервером работает")
	return nil
}

// finish delivers the result. It matters more than any report: without it the
// PC would boot WinPE again, so the agent tries for a while.
func (w *worker) finish(id int64, ok bool, message string) error {
	var last error
	for range finishTries {
		err := w.client.finish(id, ok, message, w.rec.Result())
		if err == nil || errors.Is(err, errTaskGone) {
			return nil
		}
		last = err
		w.sleep(retryDelay)
	}
	return fmt.Errorf("не удалось сообщить результат: %w", last)
}

func (w *worker) setStage(n int) {
	w.screen.update(func(st *ui.State) { st.Stages = stages(w.names, n, ui.StageActive) })
}

func (w *worker) showResult(title, detail string) {
	w.screen.update(func(st *ui.State) {
		st.Phase, st.Percent, st.Center = ui.PhaseDone, 100, "100%"
		st.Caption, st.Title, st.Detail = "", title, detail
		st.Stages = stages(w.names, len(w.names), ui.StageDone)
	})
}

func (w *worker) showError(err error) {
	w.screen.log("ошибка: " + err.Error())
	w.screen.update(func(st *ui.State) {
		current := stageNetwork
		for i, s := range st.Stages {
			if s.Status == ui.StageActive {
				current = i
			}
		}
		st.Phase, st.Center, st.Caption = ui.PhaseFailed, "!", "остановлено"
		st.Percent = max(st.Percent, 0)
		st.Title, st.Detail = "Не получилось", err.Error()
		st.Hint = "R — повторить · B или Ctrl+Alt+Del — перезагрузить · C — командная строка"
		st.Stages = stages(w.names, current, ui.StageFailed)
	})
}

// countdown keeps the result on the screen long enough to read it.
func (w *worker) countdown() {
	for left := rebootDelay; left > 0; left-- {
		w.screen.update(func(st *ui.State) { st.Hint = fmt.Sprintf("Перезагрузка через %d с", left) })
		w.sleep(time.Second)
	}
}

// localIPv4 returns the first usable address. 169.254.x.x means DHCP has not
// answered yet, so it does not count.
func localIPv4() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip := n.IP.To4(); ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			return ip.String()
		}
	}
	return ""
}

// report tells the server about a failure, so it shows up in the log and the
// panel and not only on the screen of the PC. It is best effort: the failure may
// be the network itself.
func (w *worker) report(failure error) {
	if w.taskID == 0 {
		return
	}
	stack := recovery.StackOf(failure)
	stack = stack[:min(len(stack), maxDetails)]
	if err := w.client.failure(w.taskID, failure.Error(), stack, w.rec.Result()); err != nil {
		w.screen.log("ошибка не доставлена на сервер: " + err.Error())
	}
}
