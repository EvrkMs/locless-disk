// Command agent runs inside WinPE on a client PC: it shows the screen, gets a
// task from the server and reports on it.
//
// The server injects fogs-agent.json next to the executable. Without that file
// the agent runs on demo data, so the screen can be viewed on any Windows PC:
// keys 1-5 switch the state, Esc quits.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"fogs/agent/ui"
	"fogs/proto"
)

func init() {
	// A Win32 window belongs to the thread that created it.
	runtime.LockOSThread()
}

const (
	keyEscape     = 0x1b
	keyCtrlAltDel = -1 // reported by the window, if the system lets the keys through at all
)

func main() {
	cfg, err := loadConfig()
	switch {
	case errors.Is(err, fs.ErrNotExist):
		err = runDemo()
	case err != nil:
		err = runBroken(err)
	default:
		err = runTask(cfg)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}

// loadConfig reads the file the server injected next to the executable.
func loadConfig() (proto.AgentConfig, error) {
	var cfg proto.AgentConfig
	exe, err := os.Executable()
	if err != nil {
		return cfg, err
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(exe), proto.AgentConfigName))
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, err
	}
	if cfg.Server == "" || cfg.MAC == "" {
		return cfg, errors.New("в настройках нет адреса сервера или MAC")
	}
	return cfg, nil
}

// runTask is the real work: the screen on this thread, the task on another.
func runTask(cfg proto.AgentConfig) error {
	scr := &screen{state: ui.State{
		Host:    cfg.Host + " · " + cfg.MAC,
		Server:  "сервер " + strings.TrimPrefix(cfg.Server, "http://"),
		Percent: -1,
	}}
	var reboot atomic.Bool
	w := &worker{
		cfg:        cfg,
		client:     newClient(cfg),
		screen:     scr,
		localIP:    localIPv4,
		sleep:      time.Sleep,
		openDisk:   openSystemDisk,
		openTarget: openTarget,
		abort:      make(chan struct{}, 1),
		reboot: func() {
			reboot.Store(true)
			closeWindow()
		},
		retry: make(chan struct{}, 1),
	}
	started := false
	start := time.Now()
	paint := func(dst *image.RGBA) {
		// The window exists by the first frame, so closeWindow always has a target.
		if !started {
			started = true
			go w.run()
		}
		ui.Render(dst, scr.snapshot(), time.Since(start))
	}
	key := func(code int) bool {
		switch code {
		case 'R':
			select {
			case w.retry <- struct{}{}:
			default:
			}
		case 'C':
			go openShell()
		case 'B', keyCtrlAltDel:
			if w.failed.Load() {
				w.reboot()
			}
		case keyEscape:
			select { // only the countdown before a deploy listens to this
			case w.abort <- struct{}{}:
			default:
			}
		}
		return false // the operator cannot close the agent in the middle of a task
	}
	if err := runWindow(paint, key); err != nil {
		return err
	}
	if reboot.Load() {
		rebootWinPE()
	}
	return nil
}

// rebootWinPE restarts the PC. It only ever acts inside WinPE, whose system
// drive is X:, so a stray config file cannot reboot a working Windows.
func rebootWinPE() {
	if !strings.EqualFold(os.Getenv("SystemDrive"), "X:") {
		return
	}
	exec.Command("wpeutil", "reboot").Run()
}

// runBroken shows that the injected config is unusable.
func runBroken(cause error) error {
	s := ui.State{
		Phase: ui.PhaseFailed, Center: "!", Caption: "остановлено",
		Title: "Настройки агента повреждены", Detail: cause.Error(),
		Hint: "B или Ctrl+Alt+Del — перезагрузить · C — командная строка",
	}
	start := time.Now()
	paint := func(dst *image.RGBA) { ui.Render(dst, s, time.Since(start)) }
	reboot := false
	key := func(code int) bool {
		switch code {
		case 'C':
			go openShell()
		case 'B', keyCtrlAltDel:
			reboot = true
			return true
		}
		return false
	}
	err := runWindow(paint, key)
	if reboot {
		rebootWinPE()
	}
	return err
}

// runDemo shows the screen on fake data.
func runDemo() error {
	start := time.Now()
	phase, phaseStart := ui.PhaseIdle, start

	paint := func(dst *image.RGBA) {
		now := time.Now()
		ui.Render(dst, demoState(phase, now.Sub(phaseStart)), now.Sub(start))
	}
	key := func(code int) (quit bool) {
		if code == keyEscape {
			return true
		}
		if code >= '1' && code <= '5' {
			phase, phaseStart = ui.Phase(code-'1'), time.Now()
		}
		return false
	}
	return runWindow(paint, key)
}
