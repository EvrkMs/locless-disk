package maclist

import (
	"bytes"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	macA = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x01}
	macB = net.HardwareAddr{0xaa, 0xbb, 0xcc, 0x00, 0x00, 0x02}
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// write replaces the file and moves its mtime forward, so the change is seen
// however coarse the filesystem clock is.
func write(t *testing.T, path, text string, age time.Duration) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	when := time.Now().Add(age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
}

func open(t *testing.T, text string) (*List, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "macs.txt")
	write(t, path, text, -time.Hour)
	l, err := Open(path, discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return l, path
}

func TestParse(t *testing.T) {
	text := "\xef\xbb\xbf# club test PCs\r\n" +
		"AA-BB-CC-00-00-01   # bench\r\n" +
		"\r\n" +
		"  aa:bb:cc:00:00:02\r\n" +
		"aa:bb:cc:00:00:02\r\n"
	macs, err := parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(macs) != 2 || !macs[macA.String()] || !macs[macB.String()] {
		t.Fatalf("macs: %v", macs)
	}
}

func TestParseRejects(t *testing.T) {
	tests := []struct{ name, text, wantLine string }{
		{"garbage", "aa:bb:cc:00:00:01\nnope\n", "line 2"},
		{"two on a line", "aa:bb:cc:00:00:01 aa:bb:cc:00:00:02\n", "line 1"},
		{"long address", "\n\n00:00:00:00:fe:80:00:00\n", "line 3"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(strings.NewReader(tc.text))
			if err == nil || !strings.Contains(err.Error(), tc.wantLine) {
				t.Fatalf("got %v, want an error about %s", err, tc.wantLine)
			}
		})
	}
}

func TestEmptyPathIsAnEmptyList(t *testing.T) {
	l, err := Open("", discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if l.Contains(macA) || l.Len() != 0 {
		t.Fatal("empty list is not empty")
	}
}

func TestOpenRejects(t *testing.T) {
	dir := t.TempDir()
	if _, err := Open(filepath.Join(dir, "missing.txt"), discard()); err == nil {
		t.Fatal("Open accepted a missing file")
	}
	bad := filepath.Join(dir, "bad.txt")
	write(t, bad, "nope\n", 0)
	if _, err := Open(bad, discard()); err == nil {
		t.Fatal("Open accepted a bad file")
	}
}

func TestEditTakesEffect(t *testing.T) {
	l, path := open(t, "aa:bb:cc:00:00:01\n")
	if !l.Contains(macA) || l.Contains(macB) {
		t.Fatal("wrong initial contents")
	}
	write(t, path, "aa:bb:cc:00:00:02\n", 0)
	if l.Contains(macA) || !l.Contains(macB) {
		t.Fatal("edit was not picked up")
	}
}

func TestBadEditKeepsPreviousListAndLogsOnce(t *testing.T) {
	l, path := open(t, "aa:bb:cc:00:00:01\n")
	var out bytes.Buffer
	l.log = slog.New(slog.NewTextHandler(&out, nil))

	write(t, path, "aa:bb:cc:00:00:01\nnope\n", 0)
	for range 3 {
		if !l.Contains(macA) {
			t.Fatal("a bad edit dropped the list")
		}
	}
	if n := strings.Count(out.String(), "bad MAC list"); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, out.String())
	}

	write(t, path, "aa:bb:cc:00:00:02\n", time.Hour)
	if l.Contains(macA) || !l.Contains(macB) {
		t.Fatal("fixed file was not picked up")
	}
}

func TestDeletedFileEmptiesListAndCanReturn(t *testing.T) {
	l, path := open(t, "aa:bb:cc:00:00:01\n")
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if l.Contains(macA) {
		t.Fatal("a deleted list still answers")
	}
	write(t, path, "aa:bb:cc:00:00:01\n", 0)
	if !l.Contains(macA) {
		t.Fatal("restored file was not picked up")
	}
}
