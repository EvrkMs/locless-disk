// Package ui draws the agent screen into an image. It knows nothing about
// windows or WinPE, so the look can be developed and tested on any machine.
package ui

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"strconv"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gomedium"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Phase is what the agent is doing; it picks the accent color.
type Phase int

// Agent phases.
const (
	PhaseIdle Phase = iota
	PhaseCapture
	PhaseDeploy
	PhaseDone
	PhaseFailed
)

// StageStatus is the state of one step of a task.
type StageStatus int

// Stage statuses.
const (
	StagePending StageStatus = iota
	StageActive
	StageDone
	StageFailed
)

// Stage is one line of the step list.
type Stage struct {
	Name   string
	Status StageStatus
}

// State is everything shown on the screen.
type State struct {
	Phase   Phase
	Host    string   // top left
	Server  string   // top right
	Center  string   // big text in the ring
	Caption string   // small text under Center
	Percent float64  // ring fill, 0-100; negative shows the idle spinner
	Title   string   // heading
	Detail  string   // line under the heading
	Stages  []Stage  // step list
	Log     []string // last lines are shown
	Hint    string   // bottom right
	Rates   []Rate   // speeds under the step list, at most three
}

// Rate is one speed on the screen, such as the disk or the network.
type Rate struct {
	Label string
	Value string
}

// The screen is laid out on this reference size and scaled to the real one.
const (
	refWidth  = 1280
	refHeight = 720
)

var (
	colBackground = color.RGBA{0x14, 0x16, 0x1a, 0xff}
	colTrack      = color.RGBA{0x2f, 0x32, 0x38, 0xff}
	colText       = color.RGBA{0xe9, 0xe8, 0xe3, 0xff}
	colSoft       = color.RGBA{0xb4, 0xb2, 0xa9, 0xff}
	colMuted      = color.RGBA{0x88, 0x87, 0x80, 0xff}
	colDim        = color.RGBA{0x5f, 0x5e, 0x5a, 0xff}
	colTeal       = color.RGBA{0x5d, 0xca, 0xa5, 0xff}
	colPurple     = color.RGBA{0xaf, 0xa9, 0xec, 0xff}
	colCoral      = color.RGBA{0xf0, 0x99, 0x7b, 0xff}
)

// accent returns the color that tells the phase from across the room.
func accent(p Phase) color.RGBA {
	switch p {
	case PhaseCapture:
		return colPurple
	case PhaseDeploy, PhaseDone:
		return colTeal
	case PhaseFailed:
		return colCoral
	}
	return colMuted
}

// mix blends a towards b; 0 gives a, 1 gives b.
func mix(a, b color.RGBA, k float64) color.RGBA {
	f := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*k) }
	return color.RGBA{f(a.R, b.R), f(a.G, b.G), f(a.B, b.B), 0xff}
}

// Render draws one frame. t is the time since start and drives the animation.
func Render(dst *image.RGBA, s State, t time.Duration) {
	b := dst.Bounds()
	draw.Draw(dst, b, image.NewUniform(colBackground), image.Point{}, draw.Src)

	sec := t.Seconds()
	scale := min(float64(b.Dx())/refWidth, float64(b.Dy())/refHeight)
	// The whole layout wanders a few pixels so that a screen left on for an
	// hour does not burn in. The motion is too slow to notice.
	c := canvas{
		dst:   dst,
		scale: scale,
		ox:    (float64(b.Dx())-refWidth*scale)/2 + 7*scale*math.Sin(sec/37),
		oy:    (float64(b.Dy())-refHeight*scale)/2 + 5*scale*math.Sin(sec/53),
	}
	col := accent(s.Phase)
	breath := 0.5 + 0.5*math.Cos(2*math.Pi*sec/2.8) // the sign of life

	c.text(60, 70, s.Host, 24, regular, colMuted)
	c.textRight(1220, 70, s.Server, 24, regular, colMuted)
	serverWidth := c.width(s.Server, 24, regular)
	c.disc(1220-serverWidth-22, 62, 7, mix(colTrack, col, 0.35+0.65*breath))

	c.ring(s, col, sec)

	c.text(570, 215, s.Title, 42, medium, colText)
	c.text(570, 262, s.Detail, 26, regular, colSoft)
	for i, st := range s.Stages {
		c.stage(570, 325+float64(i)*46, st, col, breath)
	}
	for i, r := range s.Rates[:min(len(s.Rates), 3)] {
		x := 570 + float64(i)*220
		c.text(x, 508, r.Label, 20, regular, colMuted)
		c.text(x, 545, r.Value, 30, medium, colText)
	}

	c.line(60, 570, 1220, 570, 1, colTrack)
	log := s.Log
	if len(log) > 3 {
		log = log[len(log)-3:]
	}
	for i, l := range log {
		c.text(60, 608+float64(i)*32, l, 22, mono, colMuted)
	}
	c.textRight(1220, 672, s.Hint, 20, regular, colDim)

	// Full width and outside the drift: a thin progress line along the edge.
	if s.Percent > 0 {
		h := max(3, int(6*scale))
		w := int(float64(b.Dx()) * min(s.Percent, 100) / 100)
		bar := image.Rect(b.Min.X, b.Max.Y-h, b.Min.X+w, b.Max.Y)
		draw.Draw(dst, bar, image.NewUniform(col), image.Point{}, draw.Src)
	}
}

// ring draws the progress ring with its text.
func (c canvas) ring(s State, col color.RGBA, sec float64) {
	const (
		cx, cy = 300, 335
		radius = 188
		thick  = 18
	)
	c.arc(cx, cy, radius, thick, 0, 2*math.Pi, colTrack)
	switch {
	case s.Percent < 0:
		// No task: a short arc slowly circles the ring.
		start := sec * 0.6
		c.arc(cx, cy, radius, thick, start, start+0.7, col)
	case s.Percent > 0:
		c.arc(cx, cy, radius, thick, 0, 2*math.Pi*min(s.Percent, 100)/100, col)
	}

	size := 88.0
	if s.Percent < 0 {
		size = 62
	}
	y := float64(cy) + size*0.3
	if s.Caption != "" {
		y -= 16
	}
	c.textCenter(cx, y, s.Center, size, medium, colText)
	c.textCenter(cx, y+50, s.Caption, 25, regular, colMuted)
}

// stage draws one line of the step list.
func (c canvas) stage(x, y float64, st Stage, col color.RGBA, breath float64) {
	ix, iy := x+11, y-9
	text := colMuted
	switch st.Status {
	case StagePending:
		c.arc(ix, iy, 8, 2.5, 0, 2*math.Pi, colDim)
	case StageActive:
		c.disc(ix, iy, 6+2*breath, col)
		text = colText
	case StageDone:
		c.line(ix-8, iy, ix-2, iy+7, 3, col)
		c.line(ix-2, iy+7, ix+9, iy-7, 3, col)
		text = colSoft
	case StageFailed:
		c.line(ix-7, iy-7, ix+7, iy+7, 3, colCoral)
		c.line(ix+7, iy-7, ix-7, iy+7, 3, colCoral)
		text = colText
	}
	c.text(x+36, y, st.Name, 27, regular, text)
}

// canvas maps reference coordinates to the real image.
type canvas struct {
	dst    *image.RGBA
	scale  float64
	ox, oy float64
}

func (c canvas) x(v float64) float64 { return c.ox + v*c.scale }
func (c canvas) y(v float64) float64 { return c.oy + v*c.scale }

// point is a position in image pixels.
type point struct{ x, y float64 }

// polygon fills a shape with antialiased edges. The mask covers only the
// shape's bounding box, so small shapes stay cheap on big screens.
func (c canvas) polygon(pts []point, col color.RGBA) {
	minX, minY := math.Inf(1), math.Inf(1)
	maxX, maxY := math.Inf(-1), math.Inf(-1)
	for _, p := range pts {
		minX, minY = min(minX, p.x), min(minY, p.y)
		maxX, maxY = max(maxX, p.x), max(maxY, p.y)
	}
	box := image.Rect(int(math.Floor(minX)), int(math.Floor(minY)), int(math.Ceil(maxX))+1, int(math.Ceil(maxY))+1)
	if box.Empty() {
		return
	}
	z := vector.NewRasterizer(box.Dx(), box.Dy())
	for i, p := range pts {
		x, y := float32(p.x-float64(box.Min.X)), float32(p.y-float64(box.Min.Y))
		if i == 0 {
			z.MoveTo(x, y)
		} else {
			z.LineTo(x, y)
		}
	}
	z.ClosePath()
	mask := image.NewAlpha(image.Rect(0, 0, box.Dx(), box.Dy()))
	z.Draw(mask, mask.Bounds(), image.Opaque, image.Point{})
	draw.DrawMask(c.dst, box, image.NewUniform(col), image.Point{}, mask, image.Point{}, draw.Over)
}

// disc fills a circle.
func (c canvas) disc(x, y, r float64, col color.RGBA) {
	c.discAt(c.x(x), c.y(y), r*c.scale, col)
}

func (c canvas) discAt(px, py, r float64, col color.RGBA) {
	steps := max(16, int(r*2))
	pts := make([]point, steps)
	for i := range pts {
		a := 2 * math.Pi * float64(i) / float64(steps)
		pts[i] = point{px + r*math.Cos(a), py + r*math.Sin(a)}
	}
	c.polygon(pts, col)
}

// arc draws a band from angle a0 to a1, clockwise from 12 o'clock, with round
// ends unless it is a full circle.
func (c canvas) arc(x, y, r, thick, a0, a1 float64, col color.RGBA) {
	px, py := c.x(x), c.y(y)
	r *= c.scale
	half := thick * c.scale / 2
	steps := max(8, int((a1-a0)*r/3))
	at := func(a, rad float64) point {
		return point{px + rad*math.Sin(a), py - rad*math.Cos(a)}
	}
	pts := make([]point, 0, 2*steps+2)
	for i := 0; i <= steps; i++ {
		pts = append(pts, at(a0+(a1-a0)*float64(i)/float64(steps), r+half))
	}
	for i := steps; i >= 0; i-- {
		pts = append(pts, at(a0+(a1-a0)*float64(i)/float64(steps), r-half))
	}
	c.polygon(pts, col)
	if a1-a0 < 2*math.Pi {
		for _, a := range []float64{a0, a1} {
			p := at(a, r)
			c.discAt(p.x, p.y, half, col)
		}
	}
}

// line draws a straight stroke with round ends.
func (c canvas) line(x0, y0, x1, y1, width float64, col color.RGBA) {
	ax, ay, bx, by := c.x(x0), c.y(y0), c.x(x1), c.y(y1)
	half := max(width*c.scale, 1) / 2
	dx, dy := bx-ax, by-ay
	n := math.Hypot(dx, dy)
	if n == 0 {
		return
	}
	nx, ny := -dy/n*half, dx/n*half
	c.polygon([]point{{ax + nx, ay + ny}, {bx + nx, by + ny}, {bx - nx, by - ny}, {ax - nx, ay - ny}}, col)
	if width > 1 {
		c.discAt(ax, ay, half, col)
		c.discAt(bx, by, half, col)
	}
}

// Typefaces. Go fonts ship with x/image, cover Cyrillic and need nothing from
// WinPE, whose own font set is small.
type typeface int

const (
	regular typeface = iota
	medium
	mono
)

var fontData = map[typeface][]byte{regular: goregular.TTF, medium: gomedium.TTF, mono: gomono.TTF}

type faceKey struct {
	face typeface
	size int
}

var (
	facesMu sync.Mutex
	parsed  = map[typeface]*opentype.Font{}
	faces   = map[faceKey]font.Face{}
)

// face returns a cached font face at a pixel size.
func face(f typeface, size float64) font.Face {
	facesMu.Lock()
	defer facesMu.Unlock()
	key := faceKey{f, int(math.Round(size))}
	if v, ok := faces[key]; ok {
		return v
	}
	tt := parsed[f]
	if tt == nil {
		var err error
		if tt, err = opentype.Parse(fontData[f]); err != nil {
			panic("ui: embedded font " + strconv.Itoa(int(f)) + ": " + err.Error())
		}
		parsed[f] = tt
	}
	v, err := opentype.NewFace(tt, &opentype.FaceOptions{Size: float64(key.size), DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		panic("ui: font size " + strconv.Itoa(key.size) + ": " + err.Error())
	}
	faces[key] = v
	return v
}

// width returns the width of s in reference units.
func (c canvas) width(s string, size float64, f typeface) float64 {
	w := font.MeasureString(face(f, size*c.scale), s)
	return float64(w) / 64 / c.scale
}

// text draws s with its left end at x and its baseline at y.
func (c canvas) text(x, y float64, s string, size float64, f typeface, col color.RGBA) {
	d := font.Drawer{
		Dst:  c.dst,
		Src:  image.NewUniform(col),
		Face: face(f, size*c.scale),
		Dot:  fixed.Point26_6{X: fixed.Int26_6(c.x(x) * 64), Y: fixed.Int26_6(c.y(y) * 64)},
	}
	d.DrawString(s)
}

func (c canvas) textRight(x, y float64, s string, size float64, f typeface, col color.RGBA) {
	c.text(x-c.width(s, size, f), y, s, size, f, col)
}

func (c canvas) textCenter(x, y float64, s string, size float64, f typeface, col color.RGBA) {
	c.text(x-c.width(s, size, f)/2, y, s, size, f, col)
}
