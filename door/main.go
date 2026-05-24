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
	"syscall"
	"time"
)

const (
	msgHelloConsumer = 0x02
	msgFrame         = 0x10
	maxMsg           = 16 << 20 // RGB frames are larger than packed cells
)

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

// renderTrue emits 24-bit color half-blocks, redrawing only changed cells.
// Returns the new per-cell token buffer (6 bytes/cell) for the next delta.
func renderTrue(buf *bytes.Buffer, px []byte, cols, rows int, prev, glyph []byte) []byte {
	cur := make([]byte, rows*cols*6)
	lastSGR := ""
	curRow, curCol := -1, -1
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			tp := ((2*y)*cols + x) * 3
			bp := ((2*y+1)*cols + x) * 3
			ci := (y*cols + x) * 6
			copy(cur[ci:ci+3], px[tp:tp+3])
			copy(cur[ci+3:ci+6], px[bp:bp+3])
			if prev != nil && bytes.Equal(prev[ci:ci+6], cur[ci:ci+6]) {
				continue
			}
			if curRow != y || curCol != x {
				fmt.Fprintf(buf, "\x1b[%d;%dH", y+1, x+1)
			}
			sgr := fmt.Sprintf("38;2;%d;%d;%d;48;2;%d;%d;%d",
				cur[ci], cur[ci+1], cur[ci+2], cur[ci+3], cur[ci+4], cur[ci+5])
			if sgr != lastSGR {
				buf.WriteString("\x1b[" + sgr + "m")
				lastSGR = sgr
			}
			buf.Write(glyph)
			curRow, curCol = y, x+1
			if curCol >= cols {
				curRow, curCol = -1, -1
			}
		}
	}
	return cur
}

// renderCGA emits CP437 + 16-color half-blocks (the BBS path). 1 byte/cell.
func renderCGA(buf *bytes.Buffer, px []byte, cols, rows int, prev []byte, sat float64, dither bool, glyph []byte) []byte {
	cur := make([]byte, rows*cols)
	lastSGR := ""
	curRow, curCol := -1, -1
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			tp := ((2*y)*cols + x) * 3
			bp := ((2*y+1)*cols + x) * 3
			top := quantCGA(int(px[tp]), int(px[tp+1]), int(px[tp+2]), x, 2*y, sat, dither)
			bot := quantCGA(int(px[bp]), int(px[bp+1]), int(px[bp+2]), x, 2*y+1, sat, dither)
			v := byte(top<<4 | bot)
			ci := y*cols + x
			cur[ci] = v
			if prev != nil && prev[ci] == v {
				continue
			}
			if curRow != y || curCol != x {
				fmt.Fprintf(buf, "\x1b[%d;%dH", y+1, x+1)
			}
			if sgr := cgaSGR(top, bot); sgr != lastSGR {
				buf.WriteString("\x1b[" + sgr + "m")
				lastSGR = sgr
			}
			buf.Write(glyph)
			curRow, curCol = y, x+1
			if curCol >= cols {
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
func renderRamp(buf *bytes.Buffer, px []byte, cols, rows int, prev []byte, use16 bool, sat float64, dither bool, glyphs [][]byte) []byte {
	ng := len(glyphs)
	stride := 4
	if use16 {
		stride = 2
	}
	cur := make([]byte, rows*cols*stride)
	lastSGR := ""
	curRow, curCol := -1, -1
	for y := 0; y < rows; y++ {
		for x := 0; x < cols; x++ {
			tp := ((2*y)*cols + x) * 3
			bp := ((2*y+1)*cols + x) * 3
			r := (int(px[tp]) + int(px[bp])) / 2
			g := (int(px[tp+1]) + int(px[bp+1])) / 2
			b := (int(px[tp+2]) + int(px[bp+2])) / 2
			gi := (r*299 + g*587 + b*114) / 1000 * (ng - 1) / 255
			ci := (y*cols + x) * stride
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
				fmt.Fprintf(buf, "\x1b[%d;%dH", y+1, x+1)
			}
			if sgr != lastSGR {
				buf.WriteString("\x1b[")
				buf.WriteString(sgr)
				buf.WriteByte('m')
				lastSGR = sgr
			}
			buf.Write(glyphs[gi])
			curRow, curCol = y, x+1
			if curCol >= cols {
				curRow, curCol = -1, -1
			}
		}
	}
	return cur
}

// renderCaption draws caption text as a centered subtitle bar on the bottom row
// (row `rows`), overlaying the video. Non-ASCII becomes '?' in CP437.
func renderCaption(buf *bytes.Buffer, caption []byte, cols, rows int, cp437 bool) {
	runes := []rune(string(caption))
	if len(runes) > cols {
		runes = runes[len(runes)-cols:] // keep the most recent words
	}
	pad := (cols - len(runes)) / 2
	fmt.Fprintf(buf, "\x1b[%d;1H\x1b[0;37;44m", rows) // white-on-blue bar, bottom row
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
	for ; used < cols; used++ {
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
	sat := flag.Float64("saturation", 1.8, "saturation boost (color=16 only)")
	dither := flag.Bool("dither", true, "ordered dithering (color=16 only)")
	fps := flag.Float64("fps", 15.0, "max render frames/sec")
	hint := flag.Bool("hint", true, "show a brief 'Q/ESC to quit' hint at startup")
	frames := flag.Int("frames", 0, "exit after N rendered frames (0=forever)")
	iniPath := flag.String("ini", defaultINIPath(), "optional .ini config (read at launch)")
	debugPath := flag.String("debug", "", "write a diagnostic log to this file (e.g. to debug input)")
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

	conn, err := net.Dial("tcp", fmt.Sprintf("%s:%d", *host, *port))
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	if err := helloConsumer(conn, *channel); err != nil {
		fmt.Fprintf(os.Stderr, "hello: %v\n", err)
		os.Exit(1)
	}

	restoreTTY := ttyRaw() // single-key input; no-op if stdin isn't a tty
	out := os.Stdout
	// Hide cursor + DISABLE auto-wrap (DECAWM). With auto-wrap on, writing the
	// last cell of the bottom row (the caption bar fills it every frame) makes
	// the terminal scroll up one line — that's the row-count-dependent glitch.
	fmt.Fprint(out, "\x1b[?25l\x1b[?7l")
	var once sync.Once
	quit := func(code int) {
		once.Do(func() {
			restoreTTY()
			setBlock(int(out.Fd())) // restore blocking so cleanup flushes
			fmt.Fprint(out, "\x1b[?7h\x1b[0m\x1b[?25h\n") // re-enable auto-wrap
		})
		os.Exit(code)
	}

	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigc; quit(0) }()

	go func() { // q/Q/ESC/Ctrl-C, or stdin EOF (caller disconnect), ends the door
		if dbg != nil {
			dbg.Printf("stdin reader started (fd 0)")
		}
		b := make([]byte, 1)
		for {
			n, again, err := readStdin(b)
			// stdin usually shares its open-file flags with stdout, which we set
			// non-blocking for the output pacer — so "no data" just means "no key
			// right now", NOT disconnect. Poll instead of quitting.
			if again {
				time.Sleep(20 * time.Millisecond)
				continue
			}
			if err != nil {
				if dbg != nil {
					dbg.Printf("stdin read error: %v (exiting)", err)
				}
				quit(0)
			}
			if n == 0 { // EOF: caller disconnected
				if dbg != nil {
					dbg.Printf("stdin EOF (exiting)")
				}
				quit(0)
			}
			if dbg != nil {
				dbg.Printf("stdin byte: %d", b[0])
			}
			switch b[0] {
			case 'q', 'Q', 27, 3: // 27 = ESC, 3 = Ctrl-C
				quit(0)
			}
		}
	}()

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
	fd := int(out.Fd())
	setNonblock(fd)

	var prev []byte
	renderSig := ""
	rendered := 0
	hintDeadline := time.Now().Add(5 * time.Second)
	hintCleared := !*hint

	var pending []byte // the single frame currently being flushed (nil = idle)
	pendingOff := 0
	minInterval := time.Duration(float64(time.Second) / *fps)
	var lastStart time.Time

	flushed := 0 // frames fully sent since lastReport — the real (measured) fps
	lastReport := time.Now()

	for {
		didWork := false

		// 1. Keep flushing the in-flight frame (non-blocking).
		if pending != nil {
			n, werr := writeNB(fd, pending[pendingOff:])
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
				cols := int(binary.BigEndian.Uint16(f[1:3]))
				rows := int(binary.BigEndian.Uint16(f[3:5]))
				mode := int(f[5])
				rampID := int(f[6])
				capLen := int(binary.BigEndian.Uint16(f[7:9]))
				var caption []byte
				px := f[9:]
				if capLen <= len(px) {
					caption = px[:capLen]
					px = px[capLen:]
				}
				if len(px) >= 2*rows*cols*3 {
					if !hintCleared && time.Now().After(hintDeadline) {
						prev = nil
						hintCleared = true
					}
					// A change in dims/mode/ramp/color, or the caption appearing/
					// disappearing, invalidates the delta — force a full repaint.
					sig := fmt.Sprintf("%d-%d-%d-%d-%v-%v", cols, rows, mode, rampID, use16, len(caption) > 0)
					if sig != renderSig {
						prev = nil
						renderSig = sig
					}
					var buf bytes.Buffer
					if prev == nil {
						buf.WriteString("\x1b[2J\x1b[H")
					}
					if mode == 1 {
						prev = renderRamp(&buf, px, cols, rows, prev, use16, *sat, *dither, rampGlyphs(rampID, cp437enc))
					} else if use16 {
						prev = renderCGA(&buf, px, cols, rows, prev, *sat, *dither, glyph)
					} else {
						prev = renderTrue(&buf, px, cols, rows, prev, glyph)
					}
					if len(caption) > 0 {
						renderCaption(&buf, caption, cols, rows, cp437enc)
					}
					if !hintCleared {
						buf.WriteString("\x1b[1;1H\x1b[1;37;44m Press Q or ESC to quit \x1b[0m")
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
