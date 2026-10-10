// Subscribes to a channel on the fanout service and renders the full-color
// pixel grid as half-blocks (▀: top pixel = fg, bottom = bg), writing to stdout.
//
// Two independent axes:
//
//	-encoding  glyph byte: cp437 (0xDF, for SyncTERM) or utf8 (▀, U+2580)
//	-color     depth: auto (ask the terminal; default), truecolor (24-bit) or
//	           16 (CGA palette: shade glyphs + half blocks, or dithered half
//	           blocks with -shade=false)
//
// e.g. cp437 + truecolor is the right combo for a modern SyncTERM caller.
// Only changed cells are redrawn each frame (delta encoding).
package main

import (
	"bufio"

	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"github.com/hmderdoc/shadecell"
	"github.com/hmderdoc/termprobe"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	msgHelloConsumer = 0x02
	msgFrame         = 0x10
	msgAudio         = 0x11
	maxMsg           = 16 << 20 // RGB frames are larger than packed cells

	capAudio = 0x01 // consumer-hello caps bit: forward AUDIO messages to us
)

// cprProbe asks the caller's terminal for its real size: save cursor, park at
// the far corner (the terminal clamps the move to its actual width/height),
// request a cursor-position report (DSR 6), restore cursor. The terminal replies
// `ESC[<rows>;<cols>R`. We use CPR rather than the xterm window report (ESC[18t)
// because Synchronet's CTerm answers DSR but has no XTWINOPS responder, so 18t
// is silently dropped on every SyncTERM build.
const cprProbe = "\x1b7\x1b[999;999H\x1b[6n\x1b8"

// keyParser is the input-stream state machine. The same byte stream from the
// caller carries three interleaved things — quit keys, telnet IAC negotiation,
// and ESC-prefixed control sequences (notably the CPR size reports we probe
// for) — so a flat switch on the first byte no longer works: a CPR reply STARTS
// with ESC, which used to be the quit key. feed() consumes one byte and reports
// what it meant.
type keyParser struct {
	state  int
	params []byte
}

const (
	ksGround    = iota // normal keys
	ksEsc              // saw ESC; awaiting [ / O (sequence) or a lone-ESC timeout
	ksCSI              // inside ESC[ … / ESCO …, accumulating params
	ksIAC              // saw telnet IAC (0xFF)
	ksIACOpt           // saw IAC WILL/WONT/DO/DONT; consume the option byte
	ksIACSub           // inside IAC SB … subnegotiation
	ksIACSubIAC        // saw IAC inside subnegotiation; SE ends it
)

type keyEvent int

const (
	keyNone     keyEvent = iota // nothing actionable (yet)
	keyQuit                     // user asked to quit
	keyResize                   // a CPR report arrived; cols/rows give the new size
	keyAudioDSR                 // an audio DSR (ESC[=7;...n) arrived; params hold the pairs
	keyMute                     // toggle audio mute
)

// awaitingEscape reports whether a lone ESC is pending — the caller uses this to
// apply the short timeout that distinguishes "user pressed Escape" from "the
// front of a control-sequence burst".
func (k *keyParser) awaitingEscape() bool { return k.state == ksEsc }

// feed advances the parser by one byte. On keyResize, cols/rows hold the size.
func (k *keyParser) feed(c byte) (ev keyEvent, cols, rows int) {
	switch k.state {
	case ksGround:
		switch c {
		case 0xFF: // telnet IAC
			k.state = ksIAC
		case 27: // ESC — could begin a control sequence; disambiguate on next byte
			k.state = ksEsc
		case 'q', 'Q', 'x', 'X', '\r', '\n', 3: // Q / X / ENTER / Ctrl-C
			return keyQuit, 0, 0
		case 'm', 'M': // toggle audio mute (no-op when audio is off)
			return keyMute, 0, 0
		}
	case ksEsc:
		switch c {
		case '[', 'O': // CSI / SS3 — a control sequence, not a quit
			k.state, k.params = ksCSI, k.params[:0]
		default: // ESC + some other byte: treat the ESC as a quit press
			k.state = ksGround
			return keyQuit, 0, 0
		}
	case ksCSI:
		// Private-prefix bytes must be accepted as params or the sequence
		// splits mid-way and its tail leaks into ground state as fake
		// keys: '=' prefixes the audio DSR reply (ESC[=7;...n), '<' the
		// SGR mouse reports some terminals send (ESC[<0;99;25M — whose
		// trailing M would otherwise toggle mute on every click).
		if (c >= '0' && c <= '9') || c == ';' || c == '=' || c == '<' || c == '>' || c == '?' {
			k.params = append(k.params, c)
		} else { // final byte ends the sequence
			k.state = ksGround
			if c == 'R' { // cursor-position report = our size-probe reply
				if cc, rr, ok := parseCPR(k.params); ok {
					return keyResize, cc, rr
				}
			}
			if c == 'n' && len(k.params) > 0 && k.params[0] == '=' {
				return keyAudioDSR, 0, 0 // caller reads k.params via parseDSRPairs
			}
		}
	case ksIAC:
		switch {
		case c == 250: // SB — subnegotiation runs until IAC SE
			k.state = ksIACSub
		case c >= 251 && c <= 254: // WILL/WONT/DO/DONT — one option byte follows
			k.state = ksIACOpt
		default: // other 2-byte command (or escaped 0xFF) — done
			k.state = ksGround
		}
	case ksIACOpt:
		k.state = ksGround
	case ksIACSub:
		if c == 0xFF {
			k.state = ksIACSubIAC
		}
	case ksIACSubIAC:
		if c == 240 { // SE
			k.state = ksGround
		} else { // escaped IAC inside the subnegotiation; keep consuming
			k.state = ksIACSub
		}
	}
	return keyNone, 0, 0
}

// parseCPR decodes the parameter bytes of a cursor-position report (the text
// between `ESC[` and the final `R`), which arrive as "rows;cols". It returns the
// size only if both fields are numeric and within a sane range, so a stray or
// partial report can never resize the display.
func parseCPR(params []byte) (cols, rows int, ok bool) {
	f := strings.Split(string(params), ";")
	if len(f) != 2 {
		return 0, 0, false
	}
	rows, err1 := strconv.Atoi(f[0])
	cols, err2 := strconv.Atoi(f[1])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	if cols < 20 || rows < 8 || cols > 1000 || rows > 1000 {
		return 0, 0, false
	}
	return cols, rows, true
}

// CGA attribute index -> ANSI SGR color digit (swaps the red/blue bits).
var cgaToAnsi = [8]int{0, 4, 2, 6, 1, 5, 3, 7}

var cgaRGB = [16][3]int{
	{0, 0, 0}, {0, 0, 170}, {0, 170, 0}, {0, 170, 170},
	{170, 0, 0}, {170, 0, 170}, {170, 85, 0}, {170, 170, 170},
	{85, 85, 85}, {85, 85, 255}, {85, 255, 85}, {85, 255, 255},
	{255, 85, 85}, {255, 85, 255}, {255, 255, 85}, {255, 255, 255},
}

// 4x4 Bayer matrix; ordered dithering is stable frame-to-frame (no shimmer).
var bayer4 = [4][4]float64{{0, 8, 2, 10}, {12, 4, 14, 6}, {3, 11, 1, 9}, {15, 7, 13, 5}}

func readMsg(r io.Reader) ([]byte, error) {
	var lenbuf [4]byte
	if _, err := io.ReadFull(r, lenbuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenbuf[:])
	if n == 0 || n > maxMsg {
		return nil, fmt.Errorf("bad message length %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// helloConsumer subscribes to a channel. caps is the wire-v2 trailing byte
// (capAudio = also send AUDIO messages); a v1 service ignores trailing bytes.
func helloConsumer(conn net.Conn, channel string, caps byte) error {
	c := []byte(channel)
	payload := append([]byte{msgHelloConsumer, byte(len(c))}, c...)
	if caps != 0 {
		payload = append(payload, caps)
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(payload)))
	_, err := conn.Write(append(hdr[:], payload...))
	return err
}

func clampB(v float64) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return int(v)
}

// saturate boosts saturation while preserving luma (cheap, no HSV round-trip).
func saturate(r, g, b int, f float64) (int, int, int) {
	l := 0.299*float64(r) + 0.587*float64(g) + 0.114*float64(b)
	return clampB(l + (float64(r)-l)*f), clampB(l + (float64(g)-l)*f), clampB(l + (float64(b)-l)*f)
}

func nearestCGA(r, g, b int) int {
	best, bi := 1<<30, 0
	for i := 0; i < 16; i++ {
		dr, dg, db := r-cgaRGB[i][0], g-cgaRGB[i][1], b-cgaRGB[i][2]
		if d := dr*dr + dg*dg + db*db; d < best {
			best, bi = d, i
		}
	}
	return bi
}

func quantCGA(r, g, b, px, py int, sat float64, dither bool) int {
	if sat != 1.0 {
		r, g, b = saturate(r, g, b, sat)
	}
	if dither {
		off := ((bayer4[py&3][px&3]+0.5)/16.0 - 0.5) * 96.0
		r, g, b = clampB(float64(r)+off), clampB(float64(g)+off), clampB(float64(b)+off)
	}
	return nearestCGA(r, g, b)
}

func cgaSGR(top, bot int) string {
	bold := 0
	if top >= 8 {
		bold = 1
	}
	fg := 30 + cgaToAnsi[top&7]
	bg := 40 + cgaToAnsi[bot&7] // dark bg only; bright bg needs iCE color
	return fmt.Sprintf("%d;%d;%d", bold, fg, bg)
}

// sampleMaps builds nearest-neighbor lookup tables that map the destination
// (caller terminal) grid back onto the producer's source frame. colMap[tx] is
// the source pixel column for destination cell column tx; rowMap[tp] is the
// source pixel row for destination *pixel* row tp (0..2*dstRows, two per cell).
// The source frame is srcCols wide and 2*srcRows pixel-rows tall. Precomputing
// these (O(dstCols+dstRows)) keeps the per-cell render loop to table lookups.
func sampleMaps(srcCols, srcRows, dstCols, dstRows int) (colMap, rowMap []int) {
	colMap = make([]int, dstCols)
	for tx := 0; tx < dstCols; tx++ {
		colMap[tx] = tx * srcCols / dstCols
	}
	rowMap = make([]int, 2*dstRows)
	for tp := 0; tp < 2*dstRows; tp++ {
		rowMap[tp] = tp * srcRows / dstRows
	}
	return colMap, rowMap
}

// fitRegion places the source frame inside the dstCols×dstRows terminal grid.
// stretch fills the whole terminal (simplest, may distort); letterbox preserves
// the source's cols:rows ratio, centering it and leaving the margins blank. It
// returns the render-region size (w,h) and its top-left cell offset (ox,oy).
func fitRegion(srcCols, srcRows, dstCols, dstRows int, stretch bool) (w, h, ox, oy int) {
	if stretch || srcCols <= 0 || srcRows <= 0 {
		return dstCols, dstRows, 0, 0
	}
	// Largest rectangle keeping srcCols:srcRows that fits — i.e. scale by
	// min(dstCols/srcCols, dstRows/srcRows). Integer math (multiply before
	// divide) avoids float drift; clamp so a tiny source can't round out of range.
	w, h = dstCols, srcRows*dstCols/srcCols
	if h > dstRows {
		w, h = srcCols*dstRows/srcRows, dstRows
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}
	return w, h, (dstCols - w) / 2, (dstRows - h) / 2
}

// renderTrue emits 24-bit color half-blocks, redrawing only changed cells. It
// resamples the srcCols×srcRows source frame into a dstCols×dstRows render
// region placed at terminal offset (ox,oy) — letterbox margins are simply cells
// we never draw. Returns the new per-cell token buffer (6 bytes/cell, sized to
// the render region) for the next delta.
func renderTrue(buf *bytes.Buffer, px []byte, srcCols, srcRows, dstCols, dstRows, ox, oy int, prev, glyph []byte) []byte {
	colMap, rowMap := sampleMaps(srcCols, srcRows, dstCols, dstRows)
	cur := make([]byte, dstRows*dstCols*6)
	lastSGR := ""
	curRow, curCol := -1, -1
	for y := 0; y < dstRows; y++ {
		spTop, spBot := rowMap[2*y], rowMap[2*y+1]
		for x := 0; x < dstCols; x++ {
			sx := colMap[x]
			tp := (spTop*srcCols + sx) * 3
			bp := (spBot*srcCols + sx) * 3
			ci := (y*dstCols + x) * 6
			copy(cur[ci:ci+3], px[tp:tp+3])
			copy(cur[ci+3:ci+6], px[bp:bp+3])
			if prev != nil && bytes.Equal(prev[ci:ci+6], cur[ci:ci+6]) {
				continue
			}
			if curRow != y || curCol != x {
				fmt.Fprintf(buf, "\x1b[%d;%dH", y+1+oy, x+1+ox)
			}
			sgr := fmt.Sprintf("38;2;%d;%d;%d;48;2;%d;%d;%d",
				cur[ci], cur[ci+1], cur[ci+2], cur[ci+3], cur[ci+4], cur[ci+5])
			if sgr != lastSGR {
				buf.WriteString("\x1b[" + sgr + "m")
				lastSGR = sgr
			}
			buf.Write(glyph)
			curRow, curCol = y, x+1
			if curCol >= dstCols {
				curRow, curCol = -1, -1
			}
		}
	}
	return cur
}

// renderCGA emits CP437 + 16-color half-blocks (the BBS path), resampling the
// source frame into a dstCols×dstRows region at terminal offset (ox,oy). 1 byte/cell.
func renderCGA(buf *bytes.Buffer, px []byte, srcCols, srcRows, dstCols, dstRows, ox, oy int, prev []byte, sat float64, dither bool, glyph []byte) []byte {
	colMap, rowMap := sampleMaps(srcCols, srcRows, dstCols, dstRows)
	cur := make([]byte, dstRows*dstCols)
	lastSGR := ""
	curRow, curCol := -1, -1
	for y := 0; y < dstRows; y++ {
		spTop, spBot := rowMap[2*y], rowMap[2*y+1]
		for x := 0; x < dstCols; x++ {
			sx := colMap[x]
			tp := (spTop*srcCols + sx) * 3
			bp := (spBot*srcCols + sx) * 3
			top := quantCGA(int(px[tp]), int(px[tp+1]), int(px[tp+2]), x, 2*y, sat, dither)
			bot := quantCGA(int(px[bp]), int(px[bp+1]), int(px[bp+2]), x, 2*y+1, sat, dither)
			v := byte(top<<4 | bot)
			ci := y*dstCols + x
			cur[ci] = v
			if prev != nil && prev[ci] == v {
				continue
			}
			if curRow != y || curCol != x {
				fmt.Fprintf(buf, "\x1b[%d;%dH", y+1+oy, x+1+ox)
			}
			if sgr := cgaSGR(top, bot); sgr != lastSGR {
				buf.WriteString("\x1b[" + sgr + "m")
				lastSGR = sgr
			}
			buf.Write(glyph)
			curRow, curCol = y, x+1
			if curCol >= dstCols {
				curRow, curCol = -1, -1
			}
		}
	}
	return cur
}

// shadeQ is the perceptual 16-colour quantizer (github.com/hmderdoc/shadecell):
// per cell it picks a space, full block, shade glyph or half block with the
// fg/bg pair whose eye-mixed colour is closest in Oklab, so the 16-colour
// palette yields a few hundred tones instead of dithered bands. One door
// serves one caller, so a single instance is fine.
var shadeQ = shadecell.New()

// renderShade is the 16-colour path with shade glyphs, same resampling and
// region as renderCGA. Delta token per cell: [glyph, attr] (2B). Saturation
// applies before quantizing like renderCGA; no ordered dither, the shade
// glyphs do that job. cp437 selects raw CP437 bytes, otherwise the Unicode
// block characters.
func renderShade(buf *bytes.Buffer, px []byte, srcCols, srcRows, dstCols, dstRows, ox, oy int, prev []byte, sat float64, cp437 bool) []byte {
	colMap, rowMap := sampleMaps(srcCols, srcRows, dstCols, dstRows)
	cur := make([]byte, dstRows*dstCols*2)
	lastSGR := ""
	curRow, curCol := -1, -1
	for y := 0; y < dstRows; y++ {
		spTop, spBot := rowMap[2*y], rowMap[2*y+1]
		for x := 0; x < dstCols; x++ {
			sx := colMap[x]
			tp := (spTop*srcCols + sx) * 3
			bp := (spBot*srcCols + sx) * 3
			tr, tg, tb := int(px[tp]), int(px[tp+1]), int(px[tp+2])
			br, bg, bb := int(px[bp]), int(px[bp+1]), int(px[bp+2])
			if sat != 1.0 {
				tr, tg, tb = saturate(tr, tg, tb, sat)
				br, bg, bb = saturate(br, bg, bb, sat)
			}
			c := shadeQ.Pair(shadecell.RGB{R: uint8(tr), G: uint8(tg), B: uint8(tb)}, shadecell.RGB{R: uint8(br), G: uint8(bg), B: uint8(bb)})
			ci := (y*dstCols + x) * 2
			cur[ci], cur[ci+1] = c.Ch, c.Attr
			if prev != nil && prev[ci] == c.Ch && prev[ci+1] == c.Attr {
				continue
			}
			if curRow != y || curCol != x {
				fmt.Fprintf(buf, "\x1b[%d;%dH", y+1+oy, x+1+ox)
			}
			if sgr := shadecell.SGR(c.Attr); sgr != lastSGR {
				buf.WriteString("\x1b[" + sgr + "m")
				lastSGR = sgr
			}
			if cp437 {
				buf.WriteByte(c.Ch)
			} else {
				buf.WriteString(shadecell.UTF8(c.Ch))
			}
			curRow, curCol = y, x+1
			if curCol >= dstCols {
				curRow, curCol = -1, -1
			}
		}
	}
	return cur
}

// updateRate folds one busy-window sample (bytes the link accepted while it
// was the constraint) into the drain-rate estimate. First sample seeds it;
// later ones blend, favoring history for stability against bursty windows.
func updateRate(est float64, bytes int, d time.Duration) float64 {
	r := float64(bytes) / d.Seconds()
	if est == 0 {
		return r
	}
	return 0.7*est + 0.3*r
}

// clampTokens bounds the video byte budget. The ceiling is ~200ms of link but
// never less than one frame: a lower ceiling than the gate's own threshold
// makes videoBudgetOK unsatisfiable and freezes the picture for the whole
// session. The floor is the mirror image — audio spends tokens ungated, and
// without a floor a stream heavier than the link drives the budget so far
// negative that video stays dead long after the link recovers.
func clampTokens(tokens, rateEst, frameBytes float64) float64 {
	if rateEst <= 0 {
		return tokens
	}
	maxBurst := rateEst * 0.2
	if maxBurst < frameBytes {
		maxBurst = frameBytes
	}
	if tokens > maxBurst {
		return maxBurst
	}
	if tokens < -maxBurst {
		return -maxBurst
	}
	return tokens
}

// videoBudgetOK reports whether a fresh frame may be taken. rateEst == 0 means
// the link has never been the constraint, so there is nothing to govern.
func videoBudgetOK(rateEst, tokens, frameBytes float64) bool {
	return rateEst == 0 || tokens >= frameBytes
}

// splitANSIUnits cuts a rendered frame into ~target-byte units, each cut
// placed directly before an ESC byte. A well-formed paint is a run of
// complete escape sequences and complete glyph byte-runs, so a boundary
// right before an ESC can never split a sequence or a UTF-8 glyph — which
// makes it safe to interleave audio APCs between the units. Returns the
// input as a single unit when it's small or has no further ESC to cut at.
func splitANSIUnits(b []byte, target int) [][]byte {
	var units [][]byte
	for len(b) > target {
		cut := target
		for cut < len(b) && b[cut] != 0x1B {
			cut++
		}
		if cut >= len(b) {
			break
		}
		units = append(units, b[:cut])
		b = b[cut:]
	}
	return append(units, b)
}

// rampGlyphs returns the per-glyph byte sequences (dark -> light) for a ramp id,
// in the right charset (CP437 bytes vs UTF-8).
func rampGlyphs(rampID int, cp437 bool) [][]byte {
	if rampID == 1 { // shade blocks
		if cp437 {
			return [][]byte{{0x20}, {0xB0}, {0xB1}, {0xB2}, {0xDB}}
		}
		return [][]byte{[]byte(" "), []byte("░"), []byte("▒"), []byte("▓"), []byte("█")}
	}
	const ascii = " .:-=+*#%@" // ASCII, identical in both charsets
	g := make([][]byte, len(ascii))
	for i := 0; i < len(ascii); i++ {
		g[i] = []byte{ascii[i]}
	}
	return g
}

// renderRamp maps each cell's brightness to a ramp glyph, colored by the cell's
// average color. Delta token per cell: truecolor [r,g,b,glyphIdx] (4B), 16-color
// [cgaIdx,glyphIdx] (2B). Glyph is foreground only (background stays default).
func renderRamp(buf *bytes.Buffer, px []byte, srcCols, srcRows, dstCols, dstRows, ox, oy int, prev []byte, use16 bool, sat float64, dither bool, glyphs [][]byte) []byte {
	colMap, rowMap := sampleMaps(srcCols, srcRows, dstCols, dstRows)
	ng := len(glyphs)
	stride := 4
	if use16 {
		stride = 2
	}
	cur := make([]byte, dstRows*dstCols*stride)
	lastSGR := ""
	curRow, curCol := -1, -1
	for y := 0; y < dstRows; y++ {
		spTop, spBot := rowMap[2*y], rowMap[2*y+1]
		for x := 0; x < dstCols; x++ {
			sx := colMap[x]
			tp := (spTop*srcCols + sx) * 3
			bp := (spBot*srcCols + sx) * 3
			r := (int(px[tp]) + int(px[bp])) / 2
			g := (int(px[tp+1]) + int(px[bp+1])) / 2
			b := (int(px[tp+2]) + int(px[bp+2])) / 2
			gi := (r*299 + g*587 + b*114) / 1000 * (ng - 1) / 255
			ci := (y*dstCols + x) * stride
			var sgr string
			if use16 {
				idx := quantCGA(r, g, b, x, y, sat, dither)
				cur[ci], cur[ci+1] = byte(idx), byte(gi)
				bold := 0
				if idx >= 8 {
					bold = 1
				}
				sgr = fmt.Sprintf("%d;%d", bold, 30+cgaToAnsi[idx&7])
			} else {
				cur[ci], cur[ci+1], cur[ci+2], cur[ci+3] = byte(r), byte(g), byte(b), byte(gi)
				sgr = fmt.Sprintf("38;2;%d;%d;%d", r, g, b)
			}
			if prev != nil && bytes.Equal(prev[ci:ci+stride], cur[ci:ci+stride]) {
				continue
			}
			if curRow != y || curCol != x {
				fmt.Fprintf(buf, "\x1b[%d;%dH", y+1+oy, x+1+ox)
			}
			if sgr != lastSGR {
				buf.WriteString("\x1b[")
				buf.WriteString(sgr)
				buf.WriteByte('m')
				lastSGR = sgr
			}
			buf.Write(glyphs[gi])
			curRow, curCol = y, x+1
			if curCol >= dstCols {
				curRow, curCol = -1, -1
			}
		}
	}
	return cur
}

// renderCaption draws caption text as a centered subtitle bar of the given
// width at (col,row) — placed just beneath the letterboxed image (within its
// column span) rather than the screen's bottom row, so it stays with the
// content. Non-ASCII becomes '?' in CP437.
func renderCaption(buf *bytes.Buffer, caption []byte, col, row, width int, cp437 bool) {
	runes := []rune(string(caption))
	if len(runes) > width {
		runes = runes[len(runes)-width:] // keep the most recent words
	}
	pad := (width - len(runes)) / 2
	fmt.Fprintf(buf, "\x1b[%d;%dH\x1b[0;37;44m", row, col+1) // white-on-blue bar
	used := 0
	for ; used < pad; used++ {
		buf.WriteByte(' ')
	}
	for _, r := range runes {
		if r < 32 || r == 127 {
			r = ' '
		}
		switch {
		case r < 128:
			buf.WriteByte(byte(r))
		case cp437:
			buf.WriteByte('?')
		default:
			buf.WriteString(string(r))
		}
		used++
	}
	for ; used < width; used++ {
		buf.WriteByte(' ')
	}
	buf.WriteString("\x1b[0m")
}

// defaultINIPath looks for door.ini next to the executable (Synchronet's
// start-up directory), falling back to the current directory.
func defaultINIPath() string {
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "door.ini")
	}
	return "door.ini"
}

// loadINI reads a flat key=value file (# or ; comments, [sections] ignored).
// A missing file is not an error — callers just keep their defaults.
func loadINI(path string) map[string]string {
	m := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return m
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' || line[0] == '[' {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			m[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	return m
}

// ttyRaw puts stdin into non-canonical, no-echo mode so single keypresses (q,
// ESC) arrive immediately instead of being buffered until Enter — and so Ctrl-C
// comes through as a byte rather than only as SIGINT. Uses stty (no extra deps);
// if stdin isn't a tty (e.g. a pipe) it's a harmless no-op. Returns a restorer.
func ttyRaw() func() {
	saved, err := sttyOn(os.Stdin, "-g")
	if err != nil {
		return func() {} // not a tty; nothing to do
	}
	if _, err := sttyOn(os.Stdin, "-icanon", "-echo", "-isig", "min", "1", "time", "0"); err != nil {
		return func() {}
	}
	prev := strings.TrimSpace(string(saved))
	return func() { sttyOn(os.Stdin, prev) }
}

func sttyOn(tty *os.File, args ...string) ([]byte, error) {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = tty
	return cmd.Output()
}

func iniBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	}
	return false, false
}

func main() {
	host := flag.String("host", "127.0.0.1", "service host")
	port := flag.Int("port", 7601, "service consumer port")
	channel := flag.String("channel", "cam", "channel to subscribe to")
	encoding := flag.String("encoding", "cp437", "half-block glyph: cp437 (byte 0xDF) | utf8 (▀)")
	color := flag.String("color", "auto", "color depth: auto (probe the terminal) | truecolor (24-bit) | 16 (CGA/ANSI)")
	fit := flag.String("fit", "letterbox", "fit to caller terminal: letterbox (keep aspect, centered) | stretch (fill)")
	forceCols := flag.Int("termcols", 0, "force caller terminal width (0 = auto-detect via CPR probe)")
	forceRows := flag.Int("termrows", 0, "force caller terminal height (0 = auto-detect via CPR probe)")
	sat := flag.Float64("saturation", 1.8, "saturation boost (color=16 only)")
	dither := flag.Bool("dither", true, "ordered dithering (color=16 with shade=false only)")
	shade := flag.Bool("shade", true, "16-color: pick shade glyphs (░▒▓) and half blocks per cell perceptually (shadeans); false = plain dithered half blocks")
	fps := flag.Float64("fps", 15.0, "max render frames/sec")
	hint := flag.Bool("hint", true, "show a brief 'Q/X/ENTER to quit' hint at startup")
	frames := flag.Int("frames", 0, "exit after N rendered frames (0=forever)")
	iniPath := flag.String("ini", defaultINIPath(), "optional .ini config (read at launch)")
	debugPath := flag.String("debug", "", "write a diagnostic log to this file (e.g. to debug input)")
	door32Path := flag.String("door32", "DOOR32.SYS",
		"path to DOOR32.SYS dropfile (auto-used when present); empty string disables")
	flag.Parse()

	// .ini fills in any setting not given explicitly on the command line, so
	// editing the file changes behavior for the next caller without a BBS restart.
	setOnCLI := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { setOnCLI[f.Name] = true })
	ini := loadINI(*iniPath)
	fromINI := func(name string) (string, bool) {
		if setOnCLI[name] {
			return "", false
		}
		v, ok := ini[name]
		return v, ok
	}
	if v, ok := fromINI("host"); ok {
		*host = v
	}
	if v, ok := fromINI("port"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*port = n
		}
	}
	if v, ok := fromINI("channel"); ok {
		*channel = v
	}
	if v, ok := fromINI("encoding"); ok {
		*encoding = v
	}
	if v, ok := fromINI("color"); ok {
		*color = v
	}
	if v, ok := fromINI("fit"); ok {
		*fit = v
	}
	if v, ok := fromINI("termcols"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*forceCols = n
		}
	}
	if v, ok := fromINI("termrows"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			*forceRows = n
		}
	}
	if v, ok := fromINI("saturation"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			*sat = f
		}
	}
	if v, ok := fromINI("dither"); ok {
		if b, valid := iniBool(v); valid {
			*dither = b
		}
	}
	if v, ok := fromINI("shade"); ok {
		if b, valid := iniBool(v); valid {
			*shade = b
		}
	}
	if v, ok := fromINI("hint"); ok {
		if b, valid := iniBool(v); valid {
			*hint = b
		}
	}
	if v, ok := fromINI("fps"); ok {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			*fps = f
		}
	}
	if v, ok := fromINI("debug"); ok {
		*debugPath = v
	}

	var dbg *log.Logger
	if *debugPath != "" {
		if lf, err := os.OpenFile(*debugPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			dbg = log.New(lf, "", log.LstdFlags|log.Lmicroseconds)
			dbg.Printf("start: encoding=%s color=%s channel=%s %s:%d", *encoding, *color, *channel, *host, *port)
		}
	}

	// DOOR32.SYS: if the BBS dropped one and line 1 == 2 (telnet), the caller's
	// socket has been inherited to us — talk to it directly instead of stdio.
	// Anything else (missing file, comm type != telnet) falls back to stdio.
	input, output := os.Stdin, os.Stdout
	if *door32Path != "" {
		info, err := readDoor32(*door32Path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "door32: %v\n", err)
			os.Exit(1)
		}
		if info != nil && info.Comm == door32CommTelnet {
			sf := os.NewFile(uintptr(info.Handle), "door32-socket")
			if sf == nil {
				fmt.Fprintf(os.Stderr, "door32: cannot wrap socket handle %d\n", info.Handle)
				os.Exit(1)
			}
			input, output = sf, sf
			if dbg != nil {
				dbg.Printf("door32: telnet socket handle=%d bbsid=%q user=%q node=%d",
					info.Handle, info.BBSID, info.User, info.Node)
			}
		} else if info != nil && dbg != nil {
			dbg.Printf("door32: comm type=%d (not telnet) — falling back to stdio", info.Comm)
		}
	}

	// The two axes are independent: glyph encoding vs color depth.
	cp437enc := strings.EqualFold(*encoding, "cp437")
	glyph := []byte("▀") // U+2580, for UTF-8 terminals
	if cp437enc {
		glyph = []byte{0xDF} // ▀ in code page 437, for SyncTERM/classic terminals
	}
	use16, colorAuto := false, false
	switch strings.ToLower(strings.TrimSpace(*color)) {
	case "16", "cga", "ansi", "ansi16":
		use16 = true
	case "", "auto":
		colorAuto = true // decided by the terminal probe below, once the tty is raw
	}
	stretchFit := strings.EqualFold(strings.TrimSpace(*fit), "stretch")

	restoreTTY := ttyRaw() // single-key input; no-op if stdin isn't a tty
	out := output

	// Live caller terminal size, learned from CPR replies (see cprProbe). The
	// producer fans out one resolution to every consumer, so we resample each
	// frame to fit THIS caller. Default to 80×25 until the first report lands,
	// so a non-answering terminal still gets a sane picture.
	var termCols, termRows atomic.Int32
	termCols.Store(80)
	termRows.Store(25)
	// A -termcols/-termrows override pins the size and disables probing — use it
	// when the terminal won't answer CPR, or to verify resampling independently
	// of detection. Auto-detect is the default (both zero).
	autoSize := true
	if *forceCols >= 20 && *forceRows >= 8 {
		termCols.Store(int32(*forceCols))
		termRows.Store(int32(*forceRows))
		autoSize = false
	}

	// Hide cursor + DISABLE auto-wrap (DECAWM). With auto-wrap on, writing the
	// last cell of the bottom row (the caption bar fills it every frame) makes
	// the terminal scroll up one line — that's the row-count-dependent glitch.
	fmt.Fprint(out, "\x1b[?25l\x1b[?7l")

	// Establish the caller's size synchronously, BEFORE the reader goroutine
	// exists (or it would swallow the reply): the kernel ioctl for a real local
	// tty, else an ANSI round-trip over the socket. sized tracks whether we've
	// got a real answer so the settle retry below and the reader cooperate.
	var sized atomic.Bool
	if autoSize {
		if c, r, ok := localTermSize(output); ok {
			termCols.Store(int32(c))
			termRows.Store(int32(r))
			sized.Store(true)
			if dbg != nil {
				dbg.Printf("local term size %dx%d", c, r)
			}
		} else if c, r, ok := probeSize(input, out, 600*time.Millisecond); ok {
			termCols.Store(int32(c))
			termRows.Store(int32(r))
			sized.Store(true)
			if dbg != nil {
				dbg.Printf("probe term size %dx%d", c, r)
			}
		} else if dbg != nil {
			dbg.Printf("size probe: no reply, using %dx%d default", termCols.Load(), termRows.Load())
		}
	}

	// Audio feature-detect: one probe, synchronous, BEFORE the reader goroutine
	// (same rule as the size probe — the reader would swallow the DSR reply).
	// The result is cached for the whole session; a terminal without Audio
	// APCs (or with audio=off in door.ini) just gets silent video, no errors.
	audio := newAudioSession(audioConfigFromINI(ini))
	audio.detect(input, out, 500*time.Millisecond)
	if dbg != nil {
		yn := "no"
		if audio.enabled {
			yn = "yes"
		}
		dbg.Printf("libsndfile: %s (audio=%s channel=%d)", yn, audio.cfg.Mode, audio.cfg.Channel)
	}
	// Colour depth: color = auto asks the terminal in band (XTVERSION, DECRQSS,
	// XTSMGRAPHICS, Device Attributes; github.com/hmderdoc/termprobe), still
	// BEFORE the reader goroutine for the same reason as the probes above.
	// SyncTERM, fTelnet and the xterm family answer and keep 24-bit; a terminal
	// that stays silent (NetRunner, the Linux console) is a 16-colour one and
	// gets the CGA path instead of truecolor mush. Keys typed meanwhile come
	// back in Leftover and are replayed into the reader below.
	var probeLeftover []byte
	if colorAuto {
		res := termprobe.ColorDepth(out, fileTimedReader{input}, 400*time.Millisecond)
		use16 = res.Depth != termprobe.DepthTrue
		probeLeftover = res.Leftover
		if dbg != nil {
			dbg.Printf("color probe: %s -> use16=%v", res, use16)
		}
	}

	// 3dBBS handheld profile. Its intake ceiling is 8 KB per rendered frame
	// (see query3DS), and audio is the part that must not starve — so spend
	// the budget on sound and cheapen the picture. Truecolor costs a
	// ~"38;2;r;g;b;48;2;r;g;b" SGR per changed cell; the 16-colour path is a
	// few bytes, for the largest single saving available without touching the
	// producer. The fps cap only trims wasted render work: the real pacing is
	// the link governor, which now actually engages.
	if audio.is3DBBS {
		use16 = true
		if *fps > 10 {
			*fps = 10
		}
		if dbg != nil {
			dbg.Printf("3dBBS caller: 16-colour, fps<=%.0f, 8-bit audio", *fps)
		}
	}
	audio.start(out) // base volume + first drain-notify arm; no-op if disabled

	// Dial the fanout service only now, so the consumer hello can carry the
	// audio caps bit and the relay never wastes bandwidth on chunks we'd skip.
	fail := func(format string, a ...any) {
		restoreTTY()
		fmt.Fprint(out, "\x1b[?7h\x1b[0m\x1b[?25h\n")
		fmt.Fprintf(os.Stderr, format, a...)
		os.Exit(1)
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fail("dial: %v\n", err)
	}
	var caps byte
	if audio.enabled {
		caps = capAudio
	}
	if err := helloConsumer(conn, *channel, caps); err != nil {
		fail("hello: %v\n", err)
	}

	// The output pacer writes in units; quit() can fire from the input
	// goroutine while a unit is HALF-flushed. Exiting there splices the
	// cleanup into the middle of an escape sequence — fragments leak into
	// whatever the BBS runs next, and an unterminated APC swallows the
	// audio Flush below as payload (the queued tail then plays on after
	// the door is gone). outMu freezes the pacer; curTail is the
	// in-flight remainder quit must finish before anything else.
	var outMu sync.Mutex
	var curTail []byte

	var once sync.Once
	quit := func(code int) {
		once.Do(func() {
			restoreTTY()
			outMu.Lock() // freeze the pacer; held through exit
			setBlock(out)
			if len(curTail) > 0 {
				out.Write(curTail) // complete the split unit
				curTail = nil
			}
			// Close any string the stream might still have open — a lone
			// ST is inert on every terminal; an open APC would otherwise
			// eat the Flush below.
			fmt.Fprint(out, "\x1b\\")
			audio.stop(out)                               // kill the queued tail NOW, not after it plays
			fmt.Fprint(out, "\x1b[?7h\x1b[0m\x1b[?25h\n") // re-enable auto-wrap
		})
		os.Exit(code)
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigc; quit(0) }()

	// Input reader. Quit keys are Q, X, and ENTER (plus Ctrl-C); a *lone* ESC
	// still quits as a courtesy, but ESC is no longer the primary quit because a
	// CPR size report STARTS with ESC — the old `case 27: quit` would have quit
	// on the first byte of every report. keyParser does the byte-level parsing.
	go func() {
		if dbg != nil {
			dbg.Printf("input reader started (fd %d)", input.Fd())
		}
		var kp keyParser
		var escAt time.Time // when a lone ESC started pending, for its timeout
		// Read in chunks, not one byte at a time: a CPR reply (ESC[rows;colsR)
		// arrives as one burst, so reading it whole and feeding the parser the
		// full chunk parses it atomically — no inter-byte poll gaps where a
		// fragment could stall or trip the lone-ESC timeout.
		feed := func(c byte) {
			switch ev, cols, rows := kp.feed(c); ev {
			case keyQuit:
				quit(0)
			case keyResize:
				termCols.Store(int32(cols))
				termRows.Store(int32(rows))
				sized.Store(true)
				if dbg != nil {
					dbg.Printf("CPR terminal size %dx%d", cols, rows)
				}
			case keyAudioDSR:
				// Drain one-shots / poll replies re-anchor the audio
				// ledger; the render loop emits the re-arm on its tick.
				if pairs, ok := parseDSRPairs(kp.params); ok {
					audio.onDSR(pairs, time.Now())
					if dbg != nil {
						dbg.Printf("audio DSR %v", pairs)
					}
				}
			case keyMute:
				// The flush that silences playback rides the next tick.
				muted := audio.toggleMute()
				if dbg != nil {
					dbg.Printf("mute toggled: %v", muted)
				}
			}
		}
		// Keys the caller typed while the colour probe was listening.
		for _, c := range probeLeftover {
			feed(c)
		}
		b := make([]byte, 64)
		for {
			n, again, err := readInput(input, b)
			// In stdio mode, stdin usually shares its open-file flags with stdout
			// (which we set non-blocking for the output pacer); in DOOR32 socket
			// mode input == output. Either way "no data" just means "no key right
			// now", NOT disconnect — poll instead of quitting.
			if again {
				// A lone ESC followed by nothing for a short window is the user
				// pressing Escape (a CPR/arrow burst arrives back-to-back, so it
				// won't sit half-parsed this long).
				if kp.awaitingEscape() && !escAt.IsZero() && time.Since(escAt) > 80*time.Millisecond {
					quit(0)
				}
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil {
				if dbg != nil {
					dbg.Printf("input read error: %v (exiting)", err)
				}
				quit(0)
			}
			if n == 0 { // EOF: caller disconnected
				if dbg != nil {
					dbg.Printf("input EOF (exiting)")
				}
				quit(0)
			}
			if dbg != nil {
				dbg.Printf("input %d bytes: % x", n, b[:n])
			}
			for i := 0; i < n; i++ {
				feed(b[i])
			}
			// Stamp/clear the lone-ESC timer based on whether an ESC is pending
			// after consuming the whole chunk.
			if kp.awaitingEscape() {
				if escAt.IsZero() {
					escAt = time.Now()
				}
			} else {
				escAt = time.Time{}
			}
		}
	}()

	// Local-tty live resize: re-read the kernel size on SIGWINCH. This is the
	// instant, reliable path for a door run on a real terminal (e.g. iTerm2);
	// telnet callers get no SIGWINCH and rely on the CPR re-probe in the render
	// loop instead. No-op on Windows.
	if autoSize {
		winch := make(chan os.Signal, 1)
		notifyWinch(winch)
		go func() {
			for range winch {
				if c, r, ok := localTermSize(output); ok {
					termCols.Store(int32(c))
					termRows.Store(int32(r))
					sized.Store(true)
					if dbg != nil {
						dbg.Printf("SIGWINCH term size %dx%d", c, r)
					}
				}
			}
		}()
	}

	// settle: if the synchronous probe missed (slow link), keep re-sending the
	// probe and let the now-running reader catch a CPR reply, so the FIRST frame
	// is already at the real size instead of the 80×25 fallback. out is still
	// blocking here, before the non-blocking pacer takes over.
	if autoSize && !sized.Load() {
		deadline := time.Now().Add(1200 * time.Millisecond)
		for time.Now().Before(deadline) && !sized.Load() {
			fmt.Fprint(out, cprProbe)
			time.Sleep(250 * time.Millisecond)
		}
	}

	var mu sync.Mutex
	var latest []byte
	var audioIn [][]byte // AUDIO messages awaiting emission (bounded, drop-oldest)
	const audioInMax = 32
	dirty, eof := false, false
	go func() { // keep only the latest frame (drop-to-latest); queue audio
		for {
			p, err := readMsg(conn)
			if err != nil {
				mu.Lock()
				eof = true
				mu.Unlock()
				return
			}
			if len(p) == 0 {
				continue
			}
			switch p[0] {
			case msgFrame:
				mu.Lock()
				latest = p
				dirty = true
				mu.Unlock()
			case msgAudio:
				// The ledger in audioSession is the real backpressure; this
				// bound only protects memory if the render loop stalls.
				mu.Lock()
				audioIn = append(audioIn, p)
				if len(audioIn) > audioInMax {
					audioIn = audioIn[len(audioIn)-audioInMax:]
				}
				mu.Unlock()
			}
		}
	}()

	// Non-blocking output: we write a frame only once the previous one has fully
	// drained, and drop everything in between. This bounds latency to ~one frame
	// in flight instead of letting stale frames pile up in the caller's buffer.
	setNonblock(out)

	var prev []byte
	renderSig := ""
	rendered := 0
	var holdSince time.Time // how long the A/V gate has been holding video
	hintDeadline := time.Now().Add(5 * time.Second)
	hintCleared := !*hint

	// The output queue is a list of UNITS, each safe to separate from its
	// neighbors (a whole small write, or a frame segment cut at an escape
	// boundary). Audio APCs are injected at the FRONT of the queue — i.e. at
	// the next unit boundary — so a large frame paint in flight can never
	// hold the caller's audio channel hostage for its full flush time. That
	// priority inversion (audio waiting out a 10s-of-KB truecolor repaint on
	// a saturated link) starves the channel and is heard as chop.
	type outUnit struct {
		b        []byte
		frameEnd bool // completes a rendered frame (fps accounting)
	}
	var outq []outUnit
	var cur []byte // unit currently flushing (nil = none)
	curOff := 0
	curFrameEnd := false
	minInterval := time.Duration(float64(time.Second) / *fps)
	var lastStart time.Time
	// Re-probe the terminal size ~1/sec for live resize. The probe goes out as
	// its own unit, so it never splits a frame; it's NOT gated on a fresh
	// frame, so a slow link or a static stream still tracks resizes.
	nextProbe := time.Now().Add(time.Second)

	flushed := 0 // frames fully sent since lastReport — the real (measured) fps
	lastReport := time.Now()

	// Link-rate governor. Audio only stays smooth if the OS buffers BETWEEN
	// the door and the caller (our socket/pipe, Synchronet's socket) stay
	// empty — once video stuffs them, injected audio sits behind seconds of
	// queued paint and no user-space reordering can save it. So: whenever
	// writes block we are link-bound, and the bytes the link accepted over
	// that busy window measure its drain rate; video is then paced to 85% of
	// the estimate via a token bucket (audio spends tokens too but is never
	// gated — the 15% margin is what keeps the downstream buffers drained).
	// rateEst == 0 means "never been link-bound": no governing, full speed.
	var rateEst float64
	tokens := 0.0
	lastTokens := time.Now()
	var busyStart time.Time
	busyBytes := 0
	lastShort := time.Now()
	// A busy window closes only after this long with no short write — proof
	// the link stopped being the constraint rather than one unit happening
	// to fit. Comfortably longer than the gap between 4 KB unit writes.
	const busyIdle = 250 * time.Millisecond
	lastFrameBytes := 8192 // refined after each render; sizes the gate

	for {
		didWork := false

		// Refill the video byte budget from the estimated link rate. The
		// burst cap (~200ms of link) bounds how far a paint can ever get
		// ahead of the drain.
		nowTok := time.Now()
		if rateEst > 0 {
			tokens += rateEst * 0.85 * nowTok.Sub(lastTokens).Seconds()
			// The cap used to be a flat 200ms of link, which silently
			// deadlocked the gate below whenever a frame cost more than that:
			// tokens could never reach lastFrameBytes, no frame was ever
			// taken, and video froze outright for the session (logged as
			// governed sessions sitting at 0.0 fps — 116x65 did it for 871
			// straight samples). Admitting at least one frame turns that into
			// merely painting SLOWLY, at rateEst/lastFrameBytes fps, which is
			// what a governor is supposed to do.
			tokens = clampTokens(tokens, rateEst, float64(lastFrameBytes))
		}
		lastTokens = nowTok
		// A link that stopped blocking us may have gotten faster (or the
		// governor is over-throttling and never sees pressure): probe upward
		// until either writes block again or we return to ungoverned.
		if rateEst > 0 && time.Since(lastShort) > 3*time.Second {
			rateEst *= 1.5
			if rateEst > 8e6 {
				rateEst = 0
			}
			lastShort = time.Now()
		}

		// 0. Audio injection — every iteration, even mid-frame. Chunks and
		//    ledger maintenance (Update re-arm, state poll, stale-cushion
		//    flush) jump the queue at the next unit boundary.
		if audio.enabled {
			mu.Lock()
			chunks := audioIn
			audioIn = nil
			mu.Unlock()
			var abuf bytes.Buffer
			for _, c := range chunks {
				if m, ok := parseAudioMsg(c); ok {
					audio.handleChunk(&abuf, m, time.Now())
				}
			}
			audio.tick(&abuf, time.Now())
			if abuf.Len() > 0 {
				outq = append([]outUnit{{b: abuf.Bytes()}}, outq...)
				tokens -= float64(abuf.Len()) // audio spends budget, ungated
			}
		}

		// 1. Keep flushing the in-flight unit; pull the next when done.
		if cur == nil && len(outq) > 0 {
			cur, curOff, curFrameEnd = outq[0].b, 0, outq[0].frameEnd
			outq = outq[1:]
		}
		if cur != nil {
			outMu.Lock()
			n, werr := writeNB(out, cur[curOff:])
			if n > 0 {
				curOff += n
				didWork = true
			}
			if curOff < len(cur) {
				curTail = cur[curOff:] // quit() finishes this if we die here
			} else {
				curTail = nil
			}
			outMu.Unlock()
			nowW := time.Now()
			if curOff < len(cur) {
				// Short write: the link is the constraint right now. Open (or
				// extend) a busy window; what it accepts measures drain rate.
				lastShort = nowW
				if busyStart.IsZero() {
					busyStart, busyBytes = nowW, 0
				}
				busyBytes += n
				if d := nowW.Sub(busyStart); d >= 2*time.Second {
					rateEst = updateRate(rateEst, busyBytes, d)
					busyStart, busyBytes = nowW, 0
				}
			} else {
				if !busyStart.IsZero() {
					busyBytes += n
					// Keep the window OPEN across completed writes. Frames are
					// segmented into 4 KB units so audio can interleave, and a
					// unit that small usually completes in one call even on a
					// saturated link — closing the window here tore it down
					// before it could ever reach the 300ms needed to measure,
					// so rateEst stayed 0 ("never been link-bound") and video
					// ran ungoverned. That is the state the door spent 97% of
					// its logged life in, while audio chopped behind the paint
					// it was never throttling. Only a genuine lull in short
					// writes means the link stopped being the constraint.
					if d := nowW.Sub(busyStart); nowW.Sub(lastShort) >= busyIdle {
						if d >= 300*time.Millisecond {
							rateEst = updateRate(rateEst, busyBytes, d)
							if dbg != nil {
								dbg.Printf("link rate est %.0f B/s", rateEst)
							}
						}
						busyStart, busyBytes = time.Time{}, 0
					} else if d >= 2*time.Second {
						// Long steady window: fold in a measurement and roll
						// it over, so a link that never lulls still reports.
						rateEst = updateRate(rateEst, busyBytes, d)
						if dbg != nil {
							dbg.Printf("link rate est %.0f B/s", rateEst)
						}
						busyStart, busyBytes = nowW, 0
					}
				}
				cur, curOff = nil, 0
				if curFrameEnd { // probes/audio units aren't frames
					curFrameEnd = false
					flushed++
					rendered++
					if *frames > 0 && rendered >= *frames {
						break
					}
				}
			}
			if werr != nil && cur != nil {
				break // caller/pipe gone
			}
		}

		// 2. Periodically record the rate the link actually sustained.
		if dbg != nil && time.Since(lastReport) >= time.Second {
			gov := "ungoverned"
			if rateEst > 0 {
				gov = fmt.Sprintf("%.0fKB/s", rateEst/1000)
			}
			dbg.Printf("effective fps: %d (target %.0f) link=%s %s",
				flushed, *fps, gov, audio.stats(time.Now()))
			flushed = 0
			lastReport = time.Now()
		}

		// 3. Queue idle and a fresh frame is ready (and the fps cap allows)?
		//    Render the LATEST one, discarding any we skipped while the last
		//    frame drained.
		if cur == nil && len(outq) == 0 {
			// Governor gate: take a frame only when the budget covers one
			// (sized by the previous paint). An ungated frame here would
			// refill the downstream buffers and put audio behind them again.
			// The frame stays in `latest` meanwhile — drop-to-latest keeps
			// whatever we eventually paint fresh, exactly like a slow link.
			budgetOK := videoBudgetOK(rateEst, tokens, float64(lastFrameBytes))
			mu.Lock()
			e := eof
			var f []byte
			haveNew := false
			if dirty && budgetOK {
				f, haveNew, dirty = latest, true, false
			}
			mu.Unlock()
			if e {
				break
			}
			var buf bytes.Buffer
			builtFrame := false
			if haveNew && time.Since(lastStart) >= minInterval && len(f) >= 9 {
				srcCols := int(binary.BigEndian.Uint16(f[1:3]))
				srcRows := int(binary.BigEndian.Uint16(f[3:5]))
				mode := int(f[5])
				rampID := int(f[6])
				capLen := int(binary.BigEndian.Uint16(f[7:9]))
				var caption []byte
				px := f[9:]
				if capLen <= len(px) {
					caption = px[:capLen]
					px = px[capLen:]
				}
				// Destination is THIS caller's terminal (from CPR), not the
				// producer's resolution — that's what makes the door responsive.
				dstCols := int(termCols.Load())
				dstRows := int(termRows.Load())
				// Letterbox (or stretch) the source into the terminal grid.
				fitCols, fitRows, ox, oy := fitRegion(srcCols, srcRows, dstCols, dstRows, stretchFit)
				pixelBytes := 2 * srcRows * srcCols * 3
				// A/V sync gate: a v2 frame carries its capture pts after the
				// pixels. Against the audio clock (queued-ledger derived), a
				// frame > sync_slack_ms behind is dropped (fresher one is
				// coming), one > sync_slack_ms ahead is held for the next
				// tick. Audio is never stretched or re-timed to compensate.
				skipFrame := false
				if audio.enabled && len(px) >= pixelBytes+8 {
					framePts := int64(binary.BigEndian.Uint64(px[pixelBytes : pixelBytes+8]))
					verdict := audio.videoGate(framePts, time.Now())
					// Bounded hold: whatever the audio clock thinks, video
					// must never freeze for more than a second. Sync is a
					// preference; a live picture is the contract.
					if verdict == gateHold && !holdSince.IsZero() && time.Since(holdSince) > time.Second {
						verdict = gateRender
					}
					switch verdict {
					case gateDrop:
						skipFrame = true
						holdSince = time.Time{}
					case gateHold:
						skipFrame = true
						if holdSince.IsZero() {
							holdSince = time.Now()
						}
						mu.Lock()
						if !dirty { // don't clobber a newer frame
							latest, dirty = f, true
						}
						mu.Unlock()
					default:
						holdSince = time.Time{}
					}
				}
				if !skipFrame && len(px) >= pixelBytes && srcCols > 0 && srcRows > 0 {
					if !hintCleared && time.Now().After(hintDeadline) {
						prev = nil
						hintCleared = true
					}
					// A change in source dims, the caller's terminal size (which
					// moves the letterbox region), mode/ramp/color, or the caption
					// appearing/disappearing invalidates the delta — force a full
					// clear + repaint. The clear also wipes stale letterbox margins.
					sig := fmt.Sprintf("%d-%d-%d-%d-%d-%d-%v-%v",
						srcCols, srcRows, dstCols, dstRows, mode, rampID, use16, len(caption) > 0)
					if sig != renderSig {
						prev = nil
						renderSig = sig
					}
					if prev == nil {
						buf.WriteString("\x1b[2J\x1b[H")
					}
					if mode == 1 {
						prev = renderRamp(&buf, px, srcCols, srcRows, fitCols, fitRows, ox, oy, prev, use16, *sat, *dither, rampGlyphs(rampID, cp437enc))
					} else if use16 && *shade {
						prev = renderShade(&buf, px, srcCols, srcRows, fitCols, fitRows, ox, oy, prev, *sat, cp437enc)
					} else if use16 {
						prev = renderCGA(&buf, px, srcCols, srcRows, fitCols, fitRows, ox, oy, prev, *sat, *dither, glyph)
					} else {
						prev = renderTrue(&buf, px, srcCols, srcRows, fitCols, fitRows, ox, oy, prev, glyph)
					}
					if len(caption) > 0 {
						// Place the bar on the row just below the image; if the
						// image is full height (no bottom margin) overlay its last
						// row, as before. Span the image's columns, not the screen's.
						capRow := oy + fitRows + 1
						if capRow > dstRows {
							capRow = dstRows
						}
						renderCaption(&buf, caption, ox, capRow, fitCols, cp437enc)
					}
					if !hintCleared {
						hintText := " Press Q, X or ENTER to quit "
						if audio.enabled {
							hintText = " Q/X/ENTER quit | M mute "
						}
						buf.WriteString("\x1b[1;1H\x1b[1;37;44m" + hintText + "\x1b[0m")
					}
					lastStart = time.Now()
					builtFrame = true
				}
			}
			// Live-resize re-probe on a timer, NOT gated on a rendered frame:
			// rides the frame buffer if we built one this tick, else goes out as a
			// standalone unit — never splitting a frame. The reply comes back as
			// a CPR the reader turns into a resize.
			if autoSize && time.Now().After(nextProbe) {
				buf.WriteString(cprProbe)
				nextProbe = time.Now().Add(time.Second)
			}
			if buf.Len() > 0 {
				if builtFrame {
					// Segment the paint so audio can interleave mid-frame.
					segs := splitANSIUnits(buf.Bytes(), 4096)
					for i, sg := range segs {
						outq = append(outq, outUnit{b: sg, frameEnd: i == len(segs)-1})
					}
					lastFrameBytes = buf.Len()
					if lastFrameBytes < 2048 {
						lastFrameBytes = 2048 // tiny deltas mustn't open the gate wide
					}
				} else {
					outq = append(outq, outUnit{b: buf.Bytes()})
				}
				tokens -= float64(buf.Len())
				didWork = true
			}
		}

		// 4. Nothing progressed (buffer full or no fresh frame) — yield briefly.
		if !didWork {
			time.Sleep(2 * time.Millisecond)
		}
	}
	quit(0)
}
