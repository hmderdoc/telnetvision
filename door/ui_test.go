package main

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// screen is a tiny terminal model for checking what the renderers draw: it
// follows CUP, CUF, SGR 0 / 38;2 / 48;2 / 38;5 / 48;5 and the four CP437
// cell glyphs, and records each cell's visible top and bottom colour. Colours
// are keys as renderRGB defines them (packed RGB, or 1<<24|index).
type screen struct {
	cols, rows int
	top, bot   [][]uint32
	written    [][]bool
	x, y       int
	fg, bg     uint32
}

func newScreen(cols, rows int) *screen {
	s := &screen{cols: cols, rows: rows}
	for i := 0; i < rows; i++ {
		s.top = append(s.top, make([]uint32, cols))
		s.bot = append(s.bot, make([]uint32, cols))
		s.written = append(s.written, make([]bool, cols))
	}
	return s
}

func (s *screen) apply(t *testing.T, b []byte) {
	t.Helper()
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == 0x1b {
			if i+1 < len(b) && b[i+1] == '[' {
				j := i + 2
				for j < len(b) && (b[j] >= '0' && b[j] <= '9' || b[j] == ';' || b[j] == '?' || b[j] == '=') {
					j++
				}
				params, final := string(b[i+2:j]), b[j]
				s.csi(t, params, final)
				i = j
				continue
			}
			t.Fatalf("unexpected ESC sequence at %d: %q", i, b[i:min(i+8, len(b))])
		}
		if s.y < 0 || s.y >= s.rows || s.x < 0 || s.x >= s.cols {
			t.Fatalf("glyph %q written off screen at %d,%d", c, s.x, s.y)
		}
		switch c {
		case 0xDF:
			s.top[s.y][s.x], s.bot[s.y][s.x] = s.fg, s.bg
		case 0xDC:
			s.top[s.y][s.x], s.bot[s.y][s.x] = s.bg, s.fg
		case 0xDB:
			s.top[s.y][s.x], s.bot[s.y][s.x] = s.fg, s.fg
		case ' ':
			s.top[s.y][s.x], s.bot[s.y][s.x] = s.bg, s.bg
		default:
			// Text (overlays): colour it as a solid background cell.
			s.top[s.y][s.x], s.bot[s.y][s.x] = s.bg, s.bg
		}
		s.written[s.y][s.x] = true
		s.x++
	}
}

func (s *screen) csi(t *testing.T, params string, final byte) {
	t.Helper()
	nums := func() []int {
		var v []int
		for _, f := range strings.Split(params, ";") {
			n, _ := strconv.Atoi(f)
			v = append(v, n)
		}
		return v
	}
	switch final {
	case 'H':
		v := nums()
		if len(v) < 2 {
			s.x, s.y = 0, 0
			return
		}
		s.y, s.x = v[0]-1, v[1]-1
	case 'C':
		n := 1
		if params != "" {
			n, _ = strconv.Atoi(params)
		}
		s.x += n
	case 'm':
		v := nums()
		for k := 0; k < len(v); k++ {
			switch {
			case v[k] == 0:
				s.fg, s.bg = 0, 0
			case (v[k] == 38 || v[k] == 48) && k+1 < len(v) && v[k+1] == 2:
				c := uint32(v[k+2])<<16 | uint32(v[k+3])<<8 | uint32(v[k+4])
				if v[k] == 38 {
					s.fg = c
				} else {
					s.bg = c
				}
				k += 4
			case (v[k] == 38 || v[k] == 48) && k+1 < len(v) && v[k+1] == 5:
				c := 1<<24 | uint32(v[k+2])
				if v[k] == 38 {
					s.fg = c
				} else {
					s.bg = c
				}
				k += 2
			}
		}
	case 'J', 'l', 'h':
	default:
		t.Fatalf("unexpected CSI %q%c", params, final)
	}
}

// want returns the key renderRGB should show for a source pixel.
func wantKey(px []byte, i int, pal256 bool) uint32 {
	if pal256 {
		return 1<<24 | uint32(nearest256(int(px[i]), int(px[i+1]), int(px[i+2])))
	}
	return uint32(px[i])<<16 | uint32(px[i+1])<<8 | uint32(px[i+2])
}

// checkScreen asserts every cell shows exactly the source frame's colours.
func checkScreen(t *testing.T, s *screen, px []byte, cols, rows int, pal256 bool, skip func(x, y int) bool) {
	t.Helper()
	bad := 0
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			if skip != nil && skip(x, y) {
				continue
			}
			tw := wantKey(px, ((2*y)*cols+x)*3, pal256)
			bw := wantKey(px, ((2*y+1)*cols+x)*3, pal256)
			if s.top[y][x] != tw || s.bot[y][x] != bw {
				if bad < 5 {
					t.Errorf("cell %d,%d shows %06x/%06x, want %06x/%06x", x, y, s.top[y][x], s.bot[y][x], tw, bw)
				}
				bad++
			}
		}
	}
}

// pxWithFlats is makePx with flat patches, so one-colour cells, shared fg/bg
// runs and short gaps all occur.
func pxWithFlats(cols, rows int) []byte {
	px := makePx(cols, rows)
	for y := 0; y < 2*rows; y++ {
		for x := 0; x < cols; x++ {
			i := (y*cols + x) * 3
			switch {
			case x < cols/4:
				px[i], px[i+1], px[i+2] = 40, 80, 120
			case x < cols/3 && y%2 == 0:
				px[i], px[i+1], px[i+2] = 200, 10, 10
			}
		}
	}
	return px
}

// The encoding shortcuts (CUF hops, solid cells, flipped half blocks) must not
// change a single pixel: a full paint, and a delta on top of it, both leave
// the screen showing exactly the source frame.
func TestRenderRGBExact(t *testing.T) {
	const cols, rows = 80, 25
	for _, pal256 := range []bool{false, true} {
		a := pxWithFlats(cols, rows)
		b := jitter(a, 30)
		for i := 0; i < len(b); i += 3 * 7 { // leave some cells unchanged: gaps
			copy(b[i:i+3], a[i:i+3])
		}
		gl := glyphsFor(true)
		m := cornerMask(-1, -1)
		scr := newScreen(cols, rows)
		var b1 bytes.Buffer
		prev := renderRGB(&b1, a, cols, rows, cols, rows, 0, 0, nil, gl, m, 0, pal256)
		scr.apply(t, b1.Bytes())
		checkScreen(t, scr, a, cols, rows, pal256, nil)
		var b2 bytes.Buffer
		renderRGB(&b2, b, cols, rows, cols, rows, 0, 0, prev, gl, m, 0, pal256)
		scr.apply(t, b2.Bytes())
		checkScreen(t, scr, b, cols, rows, pal256, nil)
	}
}

// 256-colour output is much smaller than truecolor for the same frame.
func TestRender256Smaller(t *testing.T) {
	px := makePx(80, 25)
	gl := glyphsFor(true)
	var bt, b256 bytes.Buffer
	renderTrue(&bt, px, 80, 25, 80, 25, 0, 0, nil, gl, cornerMask(-1, -1), 0)
	render256(&b256, px, 80, 25, 80, 25, 0, 0, nil, gl, cornerMask(-1, -1), 0)
	if b256.Len()*2 > bt.Len() {
		t.Errorf("256-colour paint %d bytes vs truecolor %d: expected under half", b256.Len(), bt.Len())
	}
}

func TestNearest256(t *testing.T) {
	for _, c := range []struct {
		r, g, b int
		want    byte
	}{
		{0, 0, 0, 16}, {255, 255, 255, 231}, {255, 0, 0, 196}, {0, 0, 255, 21},
		{128, 128, 128, 244}, {95, 135, 175, 67},
	} {
		if got := nearest256(c.r, c.g, c.b); got != c.want {
			t.Errorf("nearest256(%d,%d,%d) = %d, want %d", c.r, c.g, c.b, got, c.want)
		}
	}
}

// Cells under an overlay are never written; on the frame after it goes away
// the same cells are repainted even though the picture did not change.
func TestRenderMaskSkipAndForce(t *testing.T) {
	const cols, rows = 80, 25
	px := makePx(cols, rows)
	gl := glyphsFor(true)
	box := rect{20, 5, 30, 10}
	scr := newScreen(cols, rows)
	var b1 bytes.Buffer
	prev := renderTrue(&b1, px, cols, rows, cols, rows, 0, 0, nil, gl, cellMask{cornerX: -1, cornerY: -1, skip: []rect{box}}, 0)
	scr.apply(t, b1.Bytes())
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			if box.has(x, y) && scr.written[y][x] {
				t.Fatalf("cell %d,%d under the overlay was written", x, y)
			}
		}
	}
	var b2 bytes.Buffer
	renderTrue(&b2, px, cols, rows, cols, rows, 0, 0, prev, gl, cellMask{cornerX: -1, cornerY: -1, force: []rect{box}}, 0)
	scr.apply(t, b2.Bytes())
	checkScreen(t, scr, px, cols, rows, false, nil)
	// Only the box was repainted.
	scr2 := newScreen(cols, rows)
	scr2.apply(t, b2.Bytes())
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			if scr2.written[y][x] != box.has(x, y) {
				t.Fatalf("force repaint: cell %d,%d written=%v", x, y, scr2.written[y][x])
			}
		}
	}
}

// The 16-colour renderers gate on the source colours too: camera-level
// flicker must not repaint the screen.
func TestRender16NoiseGate(t *testing.T) {
	px := makePx(80, 25)
	m := cornerMask(-1, -1)
	var a, b bytes.Buffer
	prev := renderShade(&a, px, 80, 25, 80, 25, 0, 0, nil, 1.8, true, m, 10)
	renderShade(&b, jitter(px, 3), 80, 25, 80, 25, 0, 0, prev, 1.8, true, m, 10)
	if b.Len() != 0 {
		t.Errorf("shade: +-3 jitter repainted %d bytes", b.Len())
	}
	var c, d bytes.Buffer
	prev = renderCGA(&c, px, 80, 25, 80, 25, 0, 0, nil, 1.8, true, []byte{0xDF}, m, 10)
	renderCGA(&d, jitter(px, 3), 80, 25, 80, 25, 0, 0, prev, 1.8, true, []byte{0xDF}, m, 10)
	if d.Len() != 0 {
		t.Errorf("cga: +-3 jitter repainted %d bytes", d.Len())
	}
}

func TestOverlaysFit(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {80, 25}, {40, 12}, {20, 8}, {132, 50}} {
		for _, o := range []overlay{helpOverlay(sz[0], sz[1], true), offAirOverlay(sz[0], sz[1], false), toastOverlay("Volume -9 dB", sz[0])} {
			if o.r.w == 0 {
				continue // too small to draw: allowed, draws nothing
			}
			if o.r.x < 0 || o.r.y < 0 || o.r.x+o.r.w > sz[0]-1 && o.r.y+o.r.h >= sz[1] || o.r.x+o.r.w > sz[0] || o.r.y+o.r.h > sz[1] {
				t.Errorf("%s at %dx%d: rect %v out of bounds", o.id, sz[0], sz[1], o.r)
			}
		}
	}
	h := helpOverlay(80, 24, true)
	for _, l := range helpLines {
		if !bytes.Contains(h.body, []byte(l)) {
			t.Errorf("help dialog is missing %q", l)
		}
	}
}

func TestNextFPS(t *testing.T) {
	for _, c := range [][2]float64{{15, 24}, {30, 2}, {2, 5}, {12, 15}, {60, 2}} {
		if got := nextFPS(c[0]); got != c[1] {
			t.Errorf("nextFPS(%g) = %g, want %g", c[0], got, c[1])
		}
	}
}

func TestTestCard(t *testing.T) {
	f := cardFrame(80, 24)
	if len(f) != 9+80*24*6 || f[0] != msgFrame {
		t.Fatalf("card frame len %d", len(f))
	}
	px := f[9:]
	// Top-left is 75% grey, top-right 75% blue.
	if px[0] != 191 || px[1] != 191 || px[2] != 191 {
		t.Errorf("first bar = %v", px[0:3])
	}
	i := 79 * 3
	if px[i] != 0 || px[i+1] != 0 || px[i+2] != 191 {
		t.Errorf("last bar = %v", px[i:i+3])
	}
}
