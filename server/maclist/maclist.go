// Package maclist keeps the MACs the server works with in a plain text file.
//
// The file holds one MAC per line; "#" starts a comment, so a line can name
// the PC:
//
//	aa:bb:cc:dd:ee:ff  # test PC
//	AA-BB-CC-DD-EE-00
//
// Edits take effect without a restart.
package maclist

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"
)

// List is a set of MACs backed by a file. An empty path gives an empty list.
type List struct {
	path string
	log  *slog.Logger

	mu    sync.Mutex
	macs  map[string]bool
	mtime time.Time
	size  int64
}

// Open reads the file. Unlike later edits, a problem here is an error: the
// operator is watching and a typo in the path should not look like an empty list.
func Open(path string, log *slog.Logger) (*List, error) {
	l := &List{path: path, log: log, macs: map[string]bool{}}
	if path == "" {
		return l, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	macs, err := read(path)
	if err != nil {
		return nil, err
	}
	l.macs, l.mtime, l.size = macs, info.ModTime(), info.Size()
	return l, nil
}

// Contains reports whether mac is on the list, picking up file edits first.
func (l *List) Contains(mac net.HardwareAddr) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh()
	return l.macs[mac.String()]
}

// Len returns the number of MACs on the list.
func (l *List) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh()
	return len(l.macs)
}

// refresh reloads the file if it changed. A deleted file empties the list,
// because silence is the safe state. A file that fails to parse keeps the
// previous list, so a typo cannot drop every PC in the middle of a job.
func (l *List) refresh() {
	if l.path == "" {
		return
	}
	info, err := os.Stat(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		if len(l.macs) > 0 {
			l.log.Warn("MAC list file is gone, list is now empty", "file", l.path)
		}
		l.macs, l.mtime, l.size = map[string]bool{}, time.Time{}, 0
		return
	}
	if err != nil {
		l.log.Error("cannot check MAC list, keeping the previous one", "file", l.path, "err", err)
		return
	}
	if info.ModTime().Equal(l.mtime) && info.Size() == l.size {
		return
	}
	// Remember this version even if it is bad, to report it only once.
	l.mtime, l.size = info.ModTime(), info.Size()
	macs, err := read(l.path)
	if err != nil {
		l.log.Error("bad MAC list, keeping the previous one", "file", l.path, "err", err)
		return
	}
	l.macs = macs
	l.log.Info("MAC list reloaded", "file", l.path, "macs", len(macs))
}

// read loads the list from a file.
func read(path string) (map[string]bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parse(f)
}

// parse decodes the list into a set keyed by canonical MAC.
func parse(r io.Reader) (map[string]bool, error) {
	macs := make(map[string]bool)
	sc := bufio.NewScanner(r)
	for line := 1; sc.Scan(); line++ {
		text, _, _ := strings.Cut(sc.Text(), "#")
		text = strings.TrimSpace(strings.TrimPrefix(text, "\xef\xbb\xbf")) // Notepad may add a BOM
		if text == "" {
			continue
		}
		mac, err := net.ParseMAC(text)
		if err != nil || len(mac) != 6 {
			return nil, fmt.Errorf("line %d: bad MAC %q", line, text)
		}
		macs[mac.String()] = true
	}
	return macs, sc.Err()
}
