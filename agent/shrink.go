package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// shrinkRounds is how many times a shrink asks Windows again. A round can free
// space a later round is able to use, but not forever.
const shrinkRounds = 3

// minShrinkRound is the least a round is worth running for.
const minShrinkRound = 1 << 30

// volumeShrinker is the volume a shrink works on. Windows does the work; this side
// only decides how much to ask for.
type volumeShrinker interface {
	// size reads the size of the partition from the disk.
	size() (uint64, error)
	// reclaimable asks how much the volume can give now. known is false when the
	// answer could not be read; the shrink then asks for what it wants anyway.
	reclaimable() (bytes uint64, known bool, err error)
	// shrinkBy shrinks the volume by up to mib MiB, as much as it can.
	shrinkBy(mib uint64) error
}

// shrinkVolume brings a volume down toward target. NTFS can shrink only as far as
// the last file it cannot move: the MFT, its log, shadow copies. Asking for more
// than that fails and shrinks nothing, so each round asks for what Windows says it
// can give, never more than is wanted. It returns the size reached and a line for
// the log and the metrics.
func shrinkVolume(v volumeShrinker, target uint64, log func(string)) (uint64, string, error) {
	start, err := v.size()
	if err != nil {
		return 0, "", err
	}
	size := start
	var limited []string // what Windows allowed in each round, for the summary
	for round := 1; round <= shrinkRounds && size > target; round++ {
		want := size - target
		ask := want
		can, known, err := v.reclaimable()
		switch {
		case err != nil:
			log("сжатие: не удалось узнать, сколько можно освободить: " + err.Error())
		case known:
			limited = append(limited, formatBytes(can))
			log(fmt.Sprintf("сжатие, раунд %d: Windows может освободить %s, нужно %s", round, formatBytes(can), formatBytes(want)))
			ask = min(want, can)
		}
		if ask < minShrinkRound {
			break
		}
		if err := v.shrinkBy(ask >> 20); err != nil {
			log("сжатие: " + err.Error())
			break
		}
		now, err := v.size()
		if err != nil {
			return size, "", err
		}
		if now >= size {
			break // Windows moved nothing more
		}
		size = now
	}
	summary := fmt.Sprintf("%s → %s, нужно было до %s", formatBytes(start), formatBytes(size), formatBytes(target))
	if len(limited) > 0 {
		summary += ", Windows разрешала освободить " + strings.Join(limited, ", затем ")
	}
	return size, summary, nil
}

// reclaimableMiB finds the megabytes in what "shrink querymax" printed. diskpart
// speaks the language of WinPE and its OEM code page, not UTF-8: "... is:  193 GB
// (197632 MB)" in English, "... 193 Гбайт (197 632 Мбайт)" in Russian, with
// spaces, no-break spaces or commas inside the number. The number in brackets is
// the exact one, in megabytes; the word after it is not read, as its letters may
// arrive in any code page.
var reclaimableMiB = regexp.MustCompile(`\(\s*([0-9][0-9\s,.\x{a0}\x{202f}\x{fffd}]*?)\s*[^\s0-9()]{1,8}\s*\)`)

func parseReclaimable(out string) (uint64, bool) {
	m := reclaimableMiB.FindAllStringSubmatch(out, -1)
	if len(m) == 0 {
		return 0, false
	}
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, m[len(m)-1][1])
	mib, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	return mib << 20, true
}
