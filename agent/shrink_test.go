package main

import (
	"errors"
	"strings"
	"testing"
)

func TestParseReclaimable(t *testing.T) {
	tests := []struct {
		out  string
		want uint64
		ok   bool
	}{
		{"The maximum number of reclaimable bytes is:  193 GB (197632 MB)", 197632, true},
		{"The maximum number of reclaimable bytes is:  193 GB (197,632 MB)", 197632, true},
		{"Максимальное количество повторно используемых байт:  193 Гбайт (197 632 Мбайт)", 197632, true},
		{"Максимальное количество повторно используемых байт:  512 Мбайт (512 Мбайт)", 512, true},
		{"Volume 3 is the selected volume.\r\n\r\nThe maximum number of reclaimable bytes is:   99 GB (101540 MB)\r\n", 101540, true},
		// What a Russian WinPE prints, in code page 866: "(197<no-break space>632 Мбайт)".
		{"\x8c\xa0\xaa\xe1: 193 \x83\xa1\xa0\xa9\xe2 (197\xff632 \x8c\xa1\xa0\xa9\xe2)", 197632, true},
		{"DiskPart has encountered an error: Access is denied.", 0, false},
		{"Volume (C) is the selected volume.", 0, false},
		{"", 0, false},
	}
	for _, tc := range tests {
		got, ok := parseReclaimable(tc.out)
		if ok != tc.ok || got != tc.want<<20 {
			t.Errorf("%q: %d MiB, %v; want %d, %v", tc.out, got>>20, ok, tc.want, tc.ok)
		}
	}
}

// fakeVolume shrinks like NTFS: never past a file it cannot move.
type fakeVolume struct {
	sz        uint64
	floor     uint64 // the end of the last unmovable file
	known     bool
	failQuery bool
	asks      []uint64 // MiB asked per round
	failAll   bool     // diskpart fails whatever is asked
	steps     []uint64 // how far each round may go, if set: unmovable files moving
}

func (v *fakeVolume) size() (uint64, error) { return v.sz, nil }

func (v *fakeVolume) reclaimable() (uint64, bool, error) {
	if v.failQuery {
		return 0, false, errors.New("нет ответа")
	}
	return v.sz - v.floorNow(), v.known, nil
}

func (v *fakeVolume) floorNow() uint64 {
	if len(v.asks) < len(v.steps) {
		return v.steps[len(v.asks)]
	}
	return v.floor
}

func (v *fakeVolume) shrinkBy(mib uint64) error {
	floor := v.floorNow()
	v.asks = append(v.asks, mib)
	if v.failAll {
		return errors.New("diskpart: The specified shrink size is too big")
	}
	v.sz = max(v.sz-mib<<20, floor)
	return nil
}

const gib = 1 << 30

func TestShrinkAsksOnlyWhatWindowsCanGive(t *testing.T) {
	// 222 GiB with 27 GiB of data, but a file Windows cannot move at 120 GiB: the
	// old single "shrink desired=193 GiB" failed and left the whole volume.
	v := &fakeVolume{sz: 222 * gib, floor: 120 * gib, known: true}
	var lines []string
	size, summary, err := shrinkVolume(v, 30*gib, func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatalf("shrinkVolume: %v", err)
	}
	if size != 120*gib || v.asks[0] != 102*1024 {
		t.Fatalf("size %d GiB, asks %v", size/gib, v.asks)
	}
	if !strings.Contains(summary, "222.0 ГБ → 120.0 ГБ") || !strings.Contains(summary, "разрешала освободить 102.0 ГБ") {
		t.Fatalf("summary %q", summary)
	}
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "сжатие") {
		t.Fatalf("log %q", lines)
	}
}

func TestShrinkGoesOnWhileRoundsFreeMore(t *testing.T) {
	// The first round lets the second go further: Windows moved what was in the way.
	v := &fakeVolume{sz: 200 * gib, floor: 30 * gib, known: true, steps: []uint64{150 * gib, 60 * gib}}
	size, _, err := shrinkVolume(v, 40*gib, func(string) {})
	if err != nil {
		t.Fatalf("shrinkVolume: %v", err)
	}
	if size != 40*gib || len(v.asks) != 3 {
		t.Fatalf("size %d GiB after %v", size/gib, v.asks)
	}
}

func TestShrinkNeverGoesBelowTheTarget(t *testing.T) {
	v := &fakeVolume{sz: 100 * gib, floor: 10 * gib, known: true}
	size, _, _ := shrinkVolume(v, 30*gib, func(string) {})
	if size != 30*gib || len(v.asks) != 1 || v.asks[0] != 70*1024 {
		t.Fatalf("size %d GiB, asks %v", size/gib, v.asks)
	}
}

func TestShrinkWithoutAnAnswerStillTries(t *testing.T) {
	v := &fakeVolume{sz: 100 * gib, floor: 10 * gib, failQuery: true}
	size, _, err := shrinkVolume(v, 30*gib, func(string) {})
	if err != nil || size != 30*gib {
		t.Fatalf("size %d GiB, err %v", size/gib, err)
	}
}

func TestShrinkStopsWhenNothingMoves(t *testing.T) {
	v := &fakeVolume{sz: 100 * gib, floor: 99*gib + gib/2, known: true}
	size, summary, _ := shrinkVolume(v, 30*gib, func(string) {})
	if size != 100*gib || len(v.asks) != 0 {
		t.Fatalf("size %d, asks %v", size, v.asks)
	}
	if !strings.Contains(summary, "100.0 ГБ → 100.0 ГБ") {
		t.Fatalf("summary %q", summary)
	}

	failing := &fakeVolume{sz: 100 * gib, floor: 10 * gib, known: true, failAll: true}
	if size, _, _ := shrinkVolume(failing, 30*gib, func(string) {}); size != 100*gib || len(failing.asks) != 1 {
		t.Fatalf("a failing diskpart went on: %d, %v", size, failing.asks)
	}
}
