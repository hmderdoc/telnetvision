// Subscribes to a channel on the fanout service and renders the full-color
// pixel grid as half-blocks (▀: top pixel = fg, bottom = bg), writing to stdout.
//
// Two independent axes:
//   -encoding  glyph byte: cp437 (0xDF, for SyncTERM) or utf8 (▀, U+2580)
//   -color     depth: truecolor (24-bit) or 16 (CGA palette + saturation/dither)
// e.g. cp437 + truecolor is the right combo for a modern SyncTERM caller.
// Only changed cells are redrawn each frame (delta encoding).
package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
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
	maxMsg           = 16 << 20 // RGB frames are larger than packed cells
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
	keyNone   keyEvent = iota // nothing actionable (yet)
	keyQuit                   // user asked to quit
	keyResize                 // a CPR report arrived; cols/rows give the new size
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
		if (c >= '0' && c <= '9') || c == ';' {
			k.params = append(k.params, c)
		} else { // final byte ends the sequence
			k.state = ksGround
			if c == 'R' { // cursor-position report = our size-probe reply
				if cc, rr, ok := parseCPR(k.params); ok {
					return keyResize, cc, rr
				}
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

func helloConsumer(conn net.Conn, channel string) error {
	c := []byte(channel)
	payload := append([]byte{msgHelloConsumer, byte(len(c))}, c...)
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

// renderCaption draws caption text as a centered subtitle bar on the bottom row
// (row `rows`), overlaying the video. Non-ASCII becomes '?' in CP437.
func renderCaption(buf *bytes.Buffer, caption []byte, dstCols, dstRows int, cp437 bool) {
	runes := []rune(string(caption))
	if len(runes) > dstCols {
		runes = runes[len(runes)-dstCols:] // keep the most recent words
	}
	pad := (dstCols - len(runes)) / 2
	fmt.Fprintf(buf, "\x1b[%d;1H\x1b[0;37;44m", dstRows) // white-on-blue bar, bottom row
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
	for ; used < dstCols; used++ {
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
	color := flag.String("color", "truecolor", "color depth: truecolor (24-bit) | 16 (CGA/ANSI)")
	fit := flag.String("fit", "letterbox", "fit to caller terminal: letterbox (keep aspect, centered) | stretch (fill)")
	forceCols := flag.Int("termcols", 0, "force caller terminal width (0 = auto-detect via CPR probe)")
	forceRows := flag.Int("termrows", 0, "force caller terminal height (0 = auto-detect via CPR probe)")
	sat := flag.Float64("saturation", 1.8, "saturation boost (color=16 only)")
	dither := flag.Bool("dither", true, "ordered dithering (color=16 only)")
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
	use16 := false
	switch strings.ToLower(strings.TrimSpace(*color)) {
	case "16", "cga", "ansi", "ansi16":
		use16 = true
	}
	stretchFit := strings.EqualFold(strings.TrimSpace(*fit), "stretch")

	conn, err := net.Dial("tcp", net.JoinHostPort(*host, strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	if err := helloConsumer(conn, *channel); err != nil {
		fmt.Fprintf(os.Stderr, "hello: %v\n", err)
		os.Exit(1)
	}

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
	var once sync.Once
	quit := func(code int) {
		once.Do(func() {
			restoreTTY()
			setBlock(out) // restore blocking so cleanup flushes
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
		b := make([]byte, 1)
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
				dbg.Printf("input byte: %d", b[0])
			}
			switch ev, cols, rows := kp.feed(b[0]); ev {
			case keyQuit:
				quit(0)
			case keyResize:
				termCols.Store(int32(cols))
				termRows.Store(int32(rows))
				sized.Store(true)
				if dbg != nil {
					dbg.Printf("CPR terminal size %dx%d", cols, rows)
				}
			}
			// Stamp/clear the lone-ESC timer based on whether an ESC is pending.
			if kp.awaitingEscape() {
				if escAt.IsZero() {
					escAt = time.Now()
				}
			} else {
				escAt = time.Time{}
			}
		}
	}()

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
	dirty, eof := false, false
	go func() { // keep only the latest frame (drop-to-latest)
		for {
			p, err := readMsg(conn)
			if err != nil {
				mu.Lock()
				eof = true
				mu.Unlock()
				return
			}
			if len(p) > 0 && p[0] == msgFrame {
				mu.Lock()
				latest = p
				dirty = true
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
	hintDeadline := time.Now().Add(5 * time.Second)
	hintCleared := !*hint

	var pending []byte // the single frame currently being flushed (nil = idle)
	pendingOff := 0
	minInterval := time.Duration(float64(time.Second) / *fps)
	var lastStart time.Time
	// Re-probe the terminal size about once a second by riding the request on the
	// next rendered frame (so it's serialized with frame output and never splits a
	// frame's escape sequences). The startup write already sent the first probe.
	nextProbe := time.Now().Add(time.Second)

	flushed := 0 // frames fully sent since lastReport — the real (measured) fps
	lastReport := time.Now()

	for {
		didWork := false

		// 1. Keep flushing the in-flight frame (non-blocking).
		if pending != nil {
			n, werr := writeNB(out, pending[pendingOff:])
			if n > 0 {
				pendingOff += n
				didWork = true
			}
			if pendingOff >= len(pending) {
				pending, pendingOff = nil, 0
				flushed++
				rendered++
				if *frames > 0 && rendered >= *frames {
					break
				}
			} else if werr != nil {
				break // caller/pipe gone
			}
		}

		// 2. Periodically record the rate the link actually sustained.
		if dbg != nil && time.Since(lastReport) >= time.Second {
			dbg.Printf("effective fps: %d (target %.0f)", flushed, *fps)
			flushed = 0
			lastReport = time.Now()
		}

		// 3. Idle and a fresh frame is ready (and the fps cap allows)? Render the
		//    LATEST one, discarding any we skipped while the last frame drained.
		if pending == nil {
			mu.Lock()
			e, haveNew := eof, dirty
			f := latest
			if haveNew {
				dirty = false
			}
			mu.Unlock()
			if e {
				break
			}
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
				if len(px) >= 2*srcRows*srcCols*3 && srcCols > 0 && srcRows > 0 {
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
					var buf bytes.Buffer
					if prev == nil {
						buf.WriteString("\x1b[2J\x1b[H")
					}
					if mode == 1 {
						prev = renderRamp(&buf, px, srcCols, srcRows, fitCols, fitRows, ox, oy, prev, use16, *sat, *dither, rampGlyphs(rampID, cp437enc))
					} else if use16 {
						prev = renderCGA(&buf, px, srcCols, srcRows, fitCols, fitRows, ox, oy, prev, *sat, *dither, glyph)
					} else {
						prev = renderTrue(&buf, px, srcCols, srcRows, fitCols, fitRows, ox, oy, prev, glyph)
					}
					if len(caption) > 0 {
						renderCaption(&buf, caption, dstCols, dstRows, cp437enc)
					}
					if !hintCleared {
						buf.WriteString("\x1b[1;1H\x1b[1;37;44m Press Q, X or ENTER to quit \x1b[0m")
					}
					// Ride a size re-probe out with this frame (~1/sec) for live
					// resize, unless the size is pinned by -termcols/-termrows.
					if autoSize && time.Now().After(nextProbe) {
						buf.WriteString(cprProbe)
						nextProbe = time.Now().Add(time.Second)
					}
					pending = buf.Bytes()
					pendingOff = 0
					lastStart = time.Now()
					didWork = true
				}
			}
		}

		// 4. Nothing progressed (buffer full or no fresh frame) — yield briefly.
		if !didWork {
			time.Sleep(2 * time.Millisecond)
		}
	}
	quit(0)
}
