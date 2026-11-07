package main

// Caller-facing chrome drawn over the video: the help dialog, a one-line
// status toast, and the off-air test card. Overlays are absolute-positioned
// text; the renderers leave the cells under them alone (cellMask.skip) and
// repaint those cells on the frame after one goes away (cellMask.force), so
// opening or closing the dialog never costs a full-screen repaint.

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// rect is a block of terminal cells, 0-based, in terminal coordinates.
type rect struct{ x, y, w, h int }

func (r rect) has(x, y int) bool { return x >= r.x && x < r.x+r.w && y >= r.y && y < r.y+r.h }

// cellMask tells a renderer which terminal cells to leave alone and which to
// repaint even when unchanged. Coordinates are terminal cells; the renderer
// adds its letterbox offset before asking.
//
// The terminal's bottom-right cell is always skipped: writing it on a
// terminal with auto-wrap on scrolls the whole screen up a row (CTerm/SyncTERM
// does this immediately, and does not implement DECAWM off, so ESC[?7l is no
// protection there). The cell is left blank; one dark corner beats a
// scrolled frame.
type cellMask struct {
	cornerX, cornerY int
	skip, force      []rect
}

func cornerMask(x, y int) cellMask { return cellMask{cornerX: x, cornerY: y} }

func (m *cellMask) skipped(x, y int) bool {
	if x == m.cornerX && y == m.cornerY {
		return true
	}
	for _, r := range m.skip {
		if r.has(x, y) {
			return true
		}
	}
	return false
}

func (m *cellMask) forced(x, y int) bool {
	for _, r := range m.force {
		if r.has(x, y) {
			return true
		}
	}
	return false
}

// overlay is one piece of chrome: where it sits and the bytes that draw it.
type overlay struct {
	id   string
	r    rect
	body []byte
}

const (
	sgrPanel = "\x1b[0;1;37;44m" // bright white on blue: the old hint bar's look
	sgrTitle = "\x1b[0;1;33;44m" // bright yellow on blue
)

// helpLines is the dialog's text, verbatim from the owner's spec.
var helpLines = []string{
	"[C]olor mode (16, 256, true)",
	"[+] Volume up",
	"[-] Volume down",
	"[M]ute",
	"[F]rame rate",
	"[?] Show this help dialog",
	"[Q]uit",
}

// boxChars returns the double-line box glyphs in the caller's encoding.
func boxChars(cp437 bool) (tl, tr, bl, br, h, v string) {
	if cp437 {
		return "\xc9", "\xbb", "\xc8", "\xbc", "\xcd", "\xba"
	}
	return "╔", "╗", "╚", "╝", "═", "║"
}

// fitText clips s to n cells and pads it with spaces to exactly n.
func fitText(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) > n {
		s = string([]rune(s)[:n])
	}
	return s + strings.Repeat(" ", n-utf8.RuneCountInString(s))
}

// boxOverlay draws a framed panel with a title in the top border, centred
// in a cols x rows terminal, clipped to fit. Text is ASCII, so it is the
// same bytes in either encoding; only the frame glyphs differ.
func boxOverlay(id, title string, lines []string, cols, rows int, cp437 bool) overlay {
	inner := len(title) + 4
	for _, l := range lines {
		if len(l)+4 > inner {
			inner = len(l) + 4
		}
	}
	w, h := inner+2, len(lines)+2
	if w > cols-1 { // never reach the last column (bottom-right scroll rule)
		w = cols - 1
	}
	if h > rows-1 {
		h = rows - 1
	}
	if w < 4 || h < 3 {
		return overlay{id: id}
	}
	inner = w - 2
	x, y := (cols-w)/2, (rows-h)/2
	tl, tr, bl, br, hz, vt := boxChars(cp437)
	var b bytes.Buffer
	pos := func(row int) { fmt.Fprintf(&b, "\x1b[%d;%dH", y+row+1, x+1) }
	// Top border with the title set into it.
	t := " " + title + " "
	if len(t) > inner {
		t = t[:inner]
	}
	left := (inner - len(t)) / 2
	pos(0)
	b.WriteString(sgrPanel + tl + strings.Repeat(hz, left))
	b.WriteString(sgrTitle + t + sgrPanel)
	b.WriteString(strings.Repeat(hz, inner-left-len(t)) + tr)
	for i := 0; i < h-2; i++ {
		pos(i + 1)
		b.WriteString(vt + fitText("  "+lines[i], inner) + vt)
	}
	pos(h - 1)
	b.WriteString(bl + strings.Repeat(hz, inner) + br + "\x1b[0m")
	return overlay{id: id, r: rect{x, y, w, h}, body: b.Bytes()}
}

func helpOverlay(cols, rows int, cp437 bool) overlay {
	lines := append([]string{""}, helpLines...)
	lines = append(lines, "", "   Press any key to continue")
	return boxOverlay("help", "TELNETVISION", lines, cols, rows, cp437)
}

func offAirOverlay(cols, rows int, cp437 bool) overlay {
	return boxOverlay("offair", "OFF AIR", []string{"", "Please stand by", ""}, cols, rows, cp437)
}

// toastOverlay is a one-line status message centred on the top row.
func toastOverlay(text string, cols int) overlay {
	t := " " + text + " "
	if len(t) > cols-1 {
		t = t[:cols-1]
	}
	x := (cols - len(t)) / 2
	body := fmt.Sprintf("\x1b[1;%dH%s%s\x1b[0m", x+1, sgrPanel, t)
	return overlay{id: "toast:" + text, r: rect{x, 0, len(t), 1}, body: []byte(body)}
}

// testCard builds an SMPTE-style colour-bar frame (the off-air picture) as
// wire pixels: cols x 2*rows RGB, the same layout the producer sends, so it
// goes through whichever renderer the caller is using.
func testCard(cols, rows int) []byte {
	bars := [7][3]byte{{191, 191, 191}, {191, 191, 0}, {0, 191, 191}, {0, 191, 0}, {191, 0, 191}, {191, 0, 0}, {0, 0, 191}}
	mid := [7][3]byte{{0, 0, 191}, {19, 19, 19}, {191, 0, 191}, {19, 19, 19}, {0, 191, 191}, {19, 19, 19}, {191, 191, 191}}
	// Bottom band: -I, white, +Q, black, then the PLUGE steps and black.
	low := [][3]byte{{0, 33, 76}, {255, 255, 255}, {50, 0, 106}, {19, 19, 19}, {9, 9, 9}, {19, 19, 19}, {29, 29, 29}, {19, 19, 19}}
	lowW := []int{5, 5, 5, 5, 2, 2, 2, 2} // in 28ths of the width
	h := 2 * rows
	px := make([]byte, cols*h*3)
	for y := 0; y < h; y++ {
		for x := 0; x < cols; x++ {
			var c [3]byte
			switch {
			case y < h*2/3:
				c = bars[x*7/cols]
			case y < h*3/4:
				c = mid[x*7/cols]
			default:
				pos, acc := x*28/cols, 0
				for i, w := range lowW {
					acc += w
					if pos < acc {
						c = low[i]
						break
					}
				}
			}
			i := (y*cols + x) * 3
			px[i], px[i+1], px[i+2] = c[0], c[1], c[2]
		}
	}
	return px
}

// xterm 256-colour quantizer. Only the 6x6x6 cube (16..231) and the grey
// ramp (232..255) are used: indices 0..15 are the terminal's own 16 colours
// and vary between clients.
var cubeLevels = [6]int{0, 95, 135, 175, 215, 255}

func nearestCubeLevel(v int) int {
	if v < 48 {
		return 0
	}
	if v < 115 {
		return 1
	}
	return (v - 35) / 40
}

func nearest256(r, g, b int) byte {
	ri, gi, bi := nearestCubeLevel(r), nearestCubeLevel(g), nearestCubeLevel(b)
	cr, cg, cb := cubeLevels[ri], cubeLevels[gi], cubeLevels[bi]
	cubeErr := (r-cr)*(r-cr) + (g-cg)*(g-cg) + (b-cb)*(b-cb)
	// Grey ramp: 8, 18, ..., 238.
	avg := (r + g + b) / 3
	gs := (avg - 3) / 10
	if gs < 0 {
		gs = 0
	}
	if gs > 23 {
		gs = 23
	}
	gv := 8 + 10*gs
	greyErr := (r-gv)*(r-gv) + (g-gv)*(g-gv) + (b-gv)*(b-gv)
	if greyErr < cubeErr {
		return byte(232 + gs)
	}
	return byte(16 + 36*ri + 6*gi + bi)
}

// Colour modes the C key cycles through, in order.
const (
	colour16 = iota
	colour256
	colourTrue
)

var colourNames = [3]string{"16 colors", "256 colors", "true color"}

const (
	splashFor   = 10 * time.Second // the startup help closes itself after this
	toastFor    = 2 * time.Second
	offAirAfter = 5 * time.Second // no frame from the relay this long = off air
)

// fpsSteps are the frame-rate caps the F key cycles through.
var fpsSteps = []float64{2, 5, 10, 15, 24, 30}

// nextFPS returns the next cap above cur, wrapping to the lowest.
func nextFPS(cur float64) float64 {
	for _, f := range fpsSteps {
		if f > cur+0.01 {
			return f
		}
	}
	return fpsSteps[0]
}

// cardFrame wraps testCard as a FRAME payload (no caption, no pts), the
// same shape the relay delivers, so the render path needs no special case.
func cardFrame(cols, rows int) []byte {
	f := make([]byte, 9, 9+cols*rows*6)
	f[0] = msgFrame
	binary.BigEndian.PutUint16(f[1:3], uint16(cols))
	binary.BigEndian.PutUint16(f[3:5], uint16(rows))
	return append(f, testCard(cols, rows)...)
}
