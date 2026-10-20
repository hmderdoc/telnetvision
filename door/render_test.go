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
		if got := len(renderTrue(&b1, px, sc, sr, dc, dr, 0, 0, nil, glyph, -1, -1, 0)); got != dc*dr*6 {
			t.Errorf("renderTrue %v->%v delta len = %d, want %d", d.src, d.dst, got, dc*dr*6)
		}
		var b2 bytes.Buffer
		if got := len(renderCGA(&b2, px, sc, sr, dc, dr, 0, 0, nil, 1.8, true, glyph, -1, -1)); got != dc*dr {
			t.Errorf("renderCGA %v->%v delta len = %d, want %d", d.src, d.dst, got, dc*dr)
		}
		var b3 bytes.Buffer
		if got := len(renderRamp(&b3, px, sc, sr, dc, dr, 0, 0, nil, false, 1.8, true, ramp, -1, -1)); got != dc*dr*4 {
			t.Errorf("renderRamp(true) %v->%v delta len = %d, want %d", d.src, d.dst, got, dc*dr*4)
		}
		var b4 bytes.Buffer
		if got := len(renderRamp(&b4, px, sc, sr, dc, dr, 0, 0, nil, true, 1.8, true, ramp, -1, -1)); got != dc*dr*2 {
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
	glyph := []byte{0xDF}
	var b1 bytes.Buffer
	prev := renderTrue(&b1, px, 80, 25, 100, 30, 0, 0, nil, glyph, -1, -1, 0)
	var b2 bytes.Buffer
	prev2 := renderTrue(&b2, px, 80, 25, 100, 30, 0, 0, prev, glyph, -1, -1, 0)
	if !bytes.Equal(prev, prev2) {
		t.Error("delta buffer changed across identical frames")
	}
	if bytes.Contains(b2.Bytes(), glyph) {
		t.Error("second render redrew cells despite no change")
	}
}

// The terminal's bottom-right cell must never be written: on terminals that
// ignore DECAWM off (CTerm) it scrolls the screen. With the region filling the
// terminal, skipCell names that corner and every renderer must leave it alone
// while still returning a full-size delta buffer.
func TestRenderSkipsCorner(t *testing.T) {
	const dc, dr = 80, 25
	px := makePx(96, 54)
	glyph := []byte{0xDF}
	ramp := rampGlyphs(0, true)
	sx, sy := skipCell(dc, dr, 0, 0)
	if sx != dc-1 || sy != dr-1 {
		t.Fatalf("skipCell = %d,%d, want %d,%d", sx, sy, dc-1, dr-1)
	}
	corner := []byte("\x1b[25;80H")
	for name, run := range map[string]func(b *bytes.Buffer) int{
		"true": func(b *bytes.Buffer) int {
			return len(renderTrue(b, px, 96, 54, dc, dr, 0, 0, nil, glyph, sx, sy, 0)) / 6
		},
		"cga": func(b *bytes.Buffer) int {
			return len(renderCGA(b, px, 96, 54, dc, dr, 0, 0, nil, 1.8, true, glyph, sx, sy))
		},
		"shade": func(b *bytes.Buffer) int {
			return len(renderShade(b, px, 96, 54, dc, dr, 0, 0, nil, 1.8, true, sx, sy)) / 2
		},
		"ramp": func(b *bytes.Buffer) int {
			return len(renderRamp(b, px, 96, 54, dc, dr, 0, 0, nil, true, 1.8, true, ramp, sx, sy)) / 2
		},
	} {
		var b bytes.Buffer
		if cells := run(&b); cells != dc*dr {
			t.Errorf("%s: delta cells = %d, want %d", name, cells, dc*dr)
		}
		if bytes.Contains(b.Bytes(), corner) {
			t.Errorf("%s: positioned the cursor on the bottom-right cell", name)
		}
		// Row 25 must still be drawn through column 79: the move to 25;79 or
		// a run that starts earlier on the row and reaches it.
		if !bytes.Contains(b.Bytes(), []byte("\x1b[25;")) {
			t.Errorf("%s: bottom row not drawn at all", name)
		}
	}
	// With the corner outside the region (letterboxed, bottom margin), nothing
	// is skipped: skipCell lands past the region and never matches.
	_, _, ox, oy := fitRegion(80, 25, 80, 60, false)
	if sx, sy := skipCell(80, 60, ox, oy); sy < 25 {
		t.Errorf("skipCell for letterboxed region = %d,%d lies inside it", sx, sy)
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
	glyph := []byte{0xDF}
	var b1 bytes.Buffer
	prev := renderTrue(&b1, px, 80, 25, 80, 25, 0, 0, nil, glyph, -1, -1, 10)
	var b2 bytes.Buffer
	prev2 := renderTrue(&b2, jitter(px, 3), 80, 25, 80, 25, 0, 0, prev, glyph, -1, -1, 10)
	if bytes.Contains(b2.Bytes(), glyph) {
		t.Error("+-3 jitter under a threshold of 10 repainted cells")
	}
	if !bytes.Equal(prev, prev2) {
		t.Error("gated cells did not keep their displayed colors")
	}
	var b3 bytes.Buffer
	renderTrue(&b3, jitter(px, 40), 80, 25, 80, 25, 0, 0, prev, glyph, -1, -1, 10)
	if !bytes.Contains(b3.Bytes(), glyph) {
		t.Error("+-40 jitter over a threshold of 10 painted nothing")
	}
	// thr 0 is the exact delta: any change paints.
	var b4 bytes.Buffer
	renderTrue(&b4, jitter(px, 1), 80, 25, 80, 25, 0, 0, prev, glyph, -1, -1, 0)
	if !bytes.Contains(b4.Bytes(), glyph) {
		t.Error("exact delta ignored a +-1 change")
	}
}

// Partial SGR: when consecutive cells share a fg or bg, only the changed half
// goes out, and a frame always opens with a full pair.
func TestTrueSGRPartial(t *testing.T) {
	var b bytes.Buffer
	var s trueSGR
	s.write(&b, []byte{1, 2, 3, 4, 5, 6})
	if got := b.String(); got != "\x1b[38;2;1;2;3;48;2;4;5;6m" {
		t.Fatalf("first SGR = %q", got)
	}
	b.Reset()
	s.write(&b, []byte{1, 2, 3, 7, 8, 9}) // same fg, new bg
	if got := b.String(); got != "\x1b[48;2;7;8;9m" {
		t.Errorf("bg-only SGR = %q", got)
	}
	b.Reset()
	s.write(&b, []byte{9, 9, 9, 7, 8, 9}) // new fg, same bg
	if got := b.String(); got != "\x1b[38;2;9;9;9m" {
		t.Errorf("fg-only SGR = %q", got)
	}
	b.Reset()
	s.write(&b, []byte{9, 9, 9, 7, 8, 9}) // unchanged
	if b.Len() != 0 {
		t.Errorf("unchanged SGR emitted %q", b.String())
	}
}
