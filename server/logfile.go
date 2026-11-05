package main

import (
	"os"
	"sync"
)

// maxLogSize is when the log moves aside. Two files at most stay on the disk.
const maxLogSize = 20 << 20

// logFile is the log of the server on disk. A service has no console, so without
// this file nothing would be left of what happened before a crash.
type logFile struct {
	mu   sync.Mutex
	path string
	file *os.File
	size int64
}

func openLog(path string) (*logFile, error) {
	l := &logFile{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *logFile) open() error {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.file, l.size = f, info.Size()
	return nil
}

// Write adds to the log. Trouble with the file must not stop the server, so a
// rotation that fails simply keeps writing where it was.
func (l *logFile) Write(b []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.size+int64(len(b)) > maxLogSize {
		l.rotate()
	}
	n, err := l.file.Write(b)
	l.size += int64(n)
	return n, err
}

func (l *logFile) rotate() {
	l.file.Close()
	os.Remove(l.path + ".1")
	os.Rename(l.path, l.path+".1")
	if err := l.open(); err != nil {
		// The old name may still be free to reopen; if not, writes fail until restart.
		l.file, _ = os.OpenFile(l.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	}
}
