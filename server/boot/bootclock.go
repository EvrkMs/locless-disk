package boot

import (
	"sync"
	"time"
)

// bootClock measures how long a PC takes from its boot script to its agent:
// firmware, iPXE, downloading WinPE and starting it. Only the server sees both
// ends, and it keeps them in memory: a restart in between loses one, nothing more. A nil
// clock measures nothing.
type bootClock struct {
	mu   sync.Mutex
	now  func() time.Time
	sent map[string]time.Time    // MAC → when the PC got the script that starts WinPE
	took map[int64]time.Duration // task → how long the last boot of its PC took
}

func newBootClock() *bootClock {
	return &bootClock{now: time.Now, sent: map[string]time.Time{}, took: map[int64]time.Duration{}}
}

// scriptSent notes that a PC was sent into WinPE now.
func (b *bootClock) scriptSent(mac string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sent[mac] = b.now()
}

// agentArrived notes the first request of the agent after a boot and returns how
// long the boot took, or false if this PC was not seen booting.
func (b *bootClock) agentArrived(mac string, task int64) (time.Duration, bool) {
	if b == nil {
		return 0, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	at, ok := b.sent[mac]
	if !ok {
		return 0, false
	}
	delete(b.sent, mac)
	took := b.now().Sub(at)
	b.took[task] = took
	return took, true
}

// bootTime returns how long the last boot for a task took, 0 if unknown.
func (b *bootClock) bootTime(task int64) time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.took[task]
}

// forget drops what is kept for a task that has ended.
func (b *bootClock) forget(task int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.took, task)
}
