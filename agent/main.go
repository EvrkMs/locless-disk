// Command agent runs inside WinPE on a client PC. So far it is only the screen,
// driven by demo data: keys 1-5 switch the state, Esc quits.
package main

import (
	"fmt"
	"image"
	"os"
	"runtime"
	"time"

	"fogs/agent/ui"
)

func init() {
	// A Win32 window belongs to the thread that created it.
	runtime.LockOSThread()
}

const keyEscape = 0x1b

func main() {
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
	if err := runWindow(paint, key); err != nil {
		fmt.Fprintln(os.Stderr, "agent:", err)
		os.Exit(1)
	}
}
