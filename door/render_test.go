package main

import (
	"bytes"
	"testing"
)

// makePx builds a srcCols×(2*srcRows) RGB pixel buffer with a position-dependent
// gradient, so resampling actually reads varied data (not a flat field).
func makePx(srcCols, srcRows int) []byte {
	px := make([]byte, 2*srcRows*srcCols*3)
	for py := 0; py < 2*srcRows; py++ {
		for x := 0; x < srcCols; x++ {
			i := (py*srcCols + x) * 3
			px[i] = byte((x * 255) / srcCols)
			px[i+1] = byte((py * 255) / (2 * srcRows))
			px[i+2] = byte((x + py) & 0xFF)
		}
	}
	return px
}

// The render path must resample any source size onto any terminal size without
// indexing out of range, and the returned delta buffer must match the DST grid.
func TestRenderResample(t *testing.T) {
	glyph := []byte{0xDF}
	gl := glyphsFor(true)
	ramp := rampGlyphs(0, true)
	for _, d := range []struct{ src, dst [2]int }{
		{[2]int{96, 54}, [2]int{80, 25}},  // downscale (the overflow case)
		{[2]int{80, 25}, [2]int{120, 36}}, // upscale
		{[2]int{40, 20}, [2]int{40, 20}},  // identity
		{[2]int{200, 100}, [2]int{20, 8}}, // extreme downscale to the floor
	} {
		sc, sr := d.src[0], d.src[1]
		dc, dr := d.dst[0], d.dst[1]
		px := makePx(sc, sr)

		var b1 bytes.Buffer
		if got := len(renderTrue(&b1, px, sc, sr, dc, dr, 0, 0, nil, gl, cornerMask(-1, -1), 0)); got != dc*dr*6 {
			t.Errorf("renderTrue %v->%v delta len = %d, want %d", d.src, d.dst, got, dc*dr*6)
		}
		var b2 bytes.Buffer
		if got := len(renderCGA(&b2, px, sc, sr, dc, dr, 0, 0, nil, 1.8, true, glyph, cornerMask(-1, -1), 10)); got != dc*dr*cgaTok {
			t.Errorf("renderCGA %v->%v delta len = %d, want %d", d.src, d.dst, got, dc*dr*cgaTok)
		}
		var b3 bytes.Buffer
		if got := len(renderRamp(&b3, px, sc, sr, dc, dr, 0, 0, nil, false, 1.8, true, ramp, cornerMask(-1, -1))); got != dc*dr*4 {
			t.Errorf("renderRamp(true) %v->%v delta len = %d, want %d", d.src, d.dst, got, dc*dr*4)
		}
		var b4 bytes.Buffer
		if got := len(renderRamp(&b4, px, sc, sr, dc, dr, 0, 0, nil, true, 1.8, true, ramp, cornerMask(-1, -1))); got != dc*dr*2 {
			t.Errorf("renderRamp(16) %v->%v delta len = %d, want %d", d.src, d.dst, got, dc*dr*2)
		}
	}
}

func TestFitRegion(t *testing.T) {
	// Stretch always fills the whole terminal, no offset.
	if w, h, ox, oy := fitRegion(96, 54, 80, 25, true); w != 80 || h != 25 || ox != 0 || oy != 0 {
		t.Errorf("stretch = %d,%d,%d,%d, want 80,25,0,0", w, h, ox, oy)
	}
	// Letterbox 96x54 into 80x25: height-bound (25/54 < 80/96), so h=25,
	// w=96*25/54=44, centered horizontally.
	if w, h, ox, oy := fitRegion(96, 54, 80, 25, false); w != 44 || h != 25 || ox != 18 || oy != 0 {
		t.Errorf("letterbox = %d,%d,%d,%d, want 44,25,18,0", w, h, ox, oy)
	}
	// Letterbox where width is the binding dimension: tall terminal.
	if w, h, ox, oy := fitRegion(80, 25, 80, 60, false); w != 80 || h != 25 || ox != 0 || oy != 17 {
		t.Errorf("letterbox tall = %d,%d,%d,%d, want 80,25,0,17", w, h, ox, oy)
	}
	// The fit region must never exceed the terminal in either axis.
	for _, d := range [][4]int{{96, 54, 80, 25}, {10, 200, 120, 36}, {300, 10, 40, 20}} {
		w, h, ox, oy := fitRegion(d[0], d[1], d[2], d[3], false)
		if w < 1 || h < 1 || ox+w > d[2] || oy+h > d[3] {
			t.Errorf("fitRegion%v = w=%d h=%d ox=%d oy=%d out of bounds", d, w, h, ox, oy)
		}
	}
}

// With prev==current the delta must emit no glyphs (only cursor moves at most),
// and an unchanged frame must round-trip to an identical delta buffer.
func TestRenderDeltaStable(t *testing.T) {
	px := makePx(80, 25)
	gl := glyphsFor(true)
	var b1 bytes.Buffer
	prev := renderTrue(&b1, px, 80, 25, 100, 30, 0, 0, nil, gl, cornerMask(-1, -1), 0)
	var b2 bytes.Buffer
	prev2 := renderTrue(&b2, px, 80, 25, 100, 30, 0, 0, prev, gl, cornerMask(-1, -1), 0)
	if !bytes.Equal(prev, prev2) {
		t.Error("delta buffer changed across identical frames")
	}
	if b2.Len() != 0 {
		t.Errorf("second render emitted %d bytes despite no change", b2.Len())
	}
}

// The terminal's bottom-right cell must never be written: on terminals that
// ignore DECAWM off (CTerm) it scrolls the screen. With the region filling the
// terminal, every renderer must leave that corner alone while still returning
// a full-size delta buffer.
func TestRenderSkipsCorner(t *testing.T) {
	const dc, dr = 80, 25
	px := makePx(96, 54)
	glyph := []byte{0xDF}
	gl := glyphsFor(true)
	ramp := rampGlyphs(0, true)
	m := cornerMask(dc-1, dr-1)
	corner := []byte("\x1b[25;80H")
	for name, run := range map[string]func(b *bytes.Buffer) int{
		"true": func(b *bytes.Buffer) int {
			return len(renderTrue(b, px, 96, 54, dc, dr, 0, 0, nil, gl, m, 0)) / 6
		},
		"256": func(b *bytes.Buffer) int {
			return len(render256(b, px, 96, 54, dc, dr, 0, 0, nil, gl, m, 0)) / 6
		},
		"cga": func(b *bytes.Buffer) int {
			return len(renderCGA(b, px, 96, 54, dc, dr, 0, 0, nil, 1.8, true, glyph, m, 10)) / cgaTok
		},
		"shade": func(b *bytes.Buffer) int {
			return len(renderShade(b, px, 96, 54, dc, dr, 0, 0, nil, 1.8, true, m, 10)) / shadeTok
		},
		"ramp": func(b *bytes.Buffer) int {
			return len(renderRamp(b, px, 96, 54, dc, dr, 0, 0, nil, true, 1.8, true, ramp, m)) / 2
		},
	} {
		var b bytes.Buffer
		if cells := run(&b); cells != dc*dr {
			t.Errorf("%s: delta cells = %d, want %d", name, cells, dc*dr)
		}
		if bytes.Contains(b.Bytes(), corner) {
			t.Errorf("%s: positioned the cursor on the bottom-right cell", name)
		}
		if !bytes.Contains(b.Bytes(), []byte("\x1b[25;")) {
			t.Errorf("%s: bottom row not drawn at all", name)
		}
		// Nothing may be written after the cursor reaches 25;79: the cell
		// there is the last one on the row the renderer may touch.
		scr := newScreen(dc, dr)
		scr.apply(t, b.Bytes())
		if scr.written[dr-1][dc-1] {
			t.Errorf("%s: wrote the bottom-right cell", name)
		}
	}
}

// jitter returns px with every channel nudged by +-amp (deterministic).
func jitter(px []byte, amp int) []byte {
	out := make([]byte, len(px))
	for i, v := range px {
		d := (i*7)%(2*amp+1) - amp
		n := int(v) + d
		if n < 0 {
			n = 0
		}
		if n > 255 {
			n = 255
		}
		out[i] = byte(n)
	}
	return out
}

// The truecolor noise gate: sensor-level flicker inside the threshold must not
// repaint anything, and the returned buffer must keep the DISPLAYED colors (so
// the next comparison is still against the screen). Movement past the
// threshold must still paint.
func TestRenderNoiseGate(t *testing.T) {
	px := makePx(80, 25)
	gl := glyphsFor(true)
	m := cornerMask(-1, -1)
	var b1 bytes.Buffer
	prev := renderTrue(&b1, px, 80, 25, 80, 25, 0, 0, nil, gl, m, 10)
	var b2 bytes.Buffer
	prev2 := renderTrue(&b2, jitter(px, 3), 80, 25, 80, 25, 0, 0, prev, gl, m, 10)
	if b2.Len() != 0 {
		t.Error("+-3 jitter under a threshold of 10 repainted cells")
	}
	if !bytes.Equal(prev, prev2) {
		t.Error("gated cells did not keep their displayed colors")
	}
	var b3 bytes.Buffer
	renderTrue(&b3, jitter(px, 40), 80, 25, 80, 25, 0, 0, prev, gl, m, 10)
	if b3.Len() == 0 {
		t.Error("+-40 jitter over a threshold of 10 painted nothing")
	}
	// thr 0 is the exact delta: any change paints.
	var b4 bytes.Buffer
	renderTrue(&b4, jitter(px, 1), 80, 25, 80, 25, 0, 0, prev, gl, m, 0)
	if b4.Len() == 0 {
		t.Error("exact delta ignored a +-1 change")
	}
}
