package ui

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sample(p Phase, percent float64) State {
	return State{
		Phase:   p,
		Host:    "PC-12 · 04:7c:16:e7:d2:1e",
		Server:  "сервер 192.168.88.105",
		Center:  "47%",
		Caption: "112 МБ/с · 3 мин",
		Percent: percent,
		Title:   "Не удалось записать раздел C:",
		Detail:  "Сервер уведомлён",
		Stages: []Stage{
			{"Разметка диска", StageDone},
			{"Раздел EFI", StageActive},
			{"Раздел C:", StagePending},
			{"Настройка хоста", StageFailed},
		},
		Log:  []string{"один", "два", "три", "четыре"},
		Hint: "Esc — выход",
	}
}

func TestRendersAtEveryResolution(t *testing.T) {
	sizes := []image.Point{{800, 600}, {1024, 768}, {1280, 720}, {1920, 1080}, {3840, 2160}}
	for _, size := range sizes {
		for _, p := range []Phase{PhaseIdle, PhaseCapture, PhaseDeploy, PhaseDone, PhaseFailed} {
			t.Run(fmt.Sprintf("%dx%d phase %d", size.X, size.Y, p), func(t *testing.T) {
				dst := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
				Render(dst, sample(p, 47), 90*time.Second)

				if got := dst.RGBAAt(0, 0); got != colBackground {
					t.Fatalf("corner is %v, want the background", got)
				}
				drawn := 0
				for i := 0; i < len(dst.Pix); i += 4 {
					if dst.Pix[i] != colBackground.R {
						drawn++
					}
				}
				if drawn < size.X*size.Y/100 {
					t.Fatalf("only %d pixels drawn", drawn)
				}
			})
		}
	}
}

func TestProgressBarFollowsPercent(t *testing.T) {
	dst := image.NewRGBA(image.Rect(0, 0, 1000, 600))
	Render(dst, sample(PhaseDeploy, 50), 0)
	y := 599
	if got := dst.RGBAAt(400, y); got != colTeal {
		t.Fatalf("bar before 50%%: %v", got)
	}
	if got := dst.RGBAAt(600, y); got != colBackground {
		t.Fatalf("bar after 50%%: %v", got)
	}
}

func TestAccentTellsPhasesApart(t *testing.T) {
	seen := map[color.RGBA]Phase{}
	for _, p := range []Phase{PhaseIdle, PhaseCapture, PhaseDeploy, PhaseFailed} {
		c := accent(p)
		if other, ok := seen[c]; ok {
			t.Fatalf("phases %d and %d share a color", p, other)
		}
		seen[c] = p
	}
}

// TestWritePreviews saves frames for looking at the design without WinPE:
//
//	FOGS_UI_PREVIEW=some/dir go test ./agent/ui -run Previews
func TestWritePreviews(t *testing.T) {
	dir := os.Getenv("FOGS_UI_PREVIEW")
	if dir == "" {
		t.Skip("FOGS_UI_PREVIEW is not set")
	}
	idle := sample(PhaseIdle, -1)
	idle.Center, idle.Caption, idle.Stages = "Агент", "на связи", nil
	frames := map[string]State{"idle": idle, "deploy": sample(PhaseDeploy, 47), "failed": sample(PhaseFailed, 47)}
	for name, s := range frames {
		dst := image.NewRGBA(image.Rect(0, 0, 1280, 720))
		Render(dst, s, 5*time.Second)
		f, err := os.Create(filepath.Join(dir, name+".png"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if err := png.Encode(f, dst); err != nil {
			t.Fatal(err)
		}
	}
}
