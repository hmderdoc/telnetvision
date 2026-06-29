// Terminal-size detection, ported from the proven spekder/avatar_chat door
// pattern: establish the caller's size synchronously at startup — local ioctl
// for a real tty, else an ANSI round-trip over the (telnet) socket — BEFORE the
// reader goroutine exists, so the goroutine can't swallow the reply. Live
// resize afterwards rides the periodic re-probe + the reader's CPR parsing.
package main

import (
	"os"
	"time"
)

// probeSize asks the caller's terminal for its size over the I/O channel: park
// the cursor at the far corner so the terminal clamps to its real dimensions,
// then DSR cursor-position report (the only probe SyncTERM/CTerm answers), with
// xterm's window report as a fallback for real-xterm callers.
//
// MUST be called BEFORE the reader goroutine starts, or that goroutine consumes
// the ESC[rows;colsR reply instead.
func probeSize(in, out *os.File, timeout time.Duration) (cols, rows int, ok bool) {
	if c, r, found := probeWith(in, out, []byte(cprProbe), parseCursorReport, timeout); found {
		return c, r, true
	}
	if c, r, found := probeWith(in, out, []byte("\x1b[18t"), parseWindowReport, timeout); found {
		return c, r, true
	}
	return 0, 0, false
}

func probeWith(in, out *os.File, query []byte, parse func([]byte) (int, int, bool), timeout time.Duration) (cols, rows int, ok bool) {
	if _, err := out.Write(query); err != nil {
		return 0, 0, false
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 0, 128)
	scratch := make([]byte, 64)
	for time.Now().Before(deadline) && len(buf) < 256 {
		rem := time.Until(deadline)
		if rem > 50*time.Millisecond {
			rem = 50 * time.Millisecond
		}
		n, err := readTimeout(in, scratch, rem)
		if n > 0 {
			buf = append(buf, scratch[:n]...)
			if c, r, found := parse(buf); found {
				return c, r, true
			}
		} else if err != nil {
			break
		}
	}
	return 0, 0, false
}

// parseCursorReport scans a raw byte buffer for ESC [ <rows> ; <cols> R (a DSR
// cursor-position report) and returns the validated cols/rows. Unlike parseCPR
// (which decodes already-isolated params from the live reader) this tolerates
// surrounding noise, so it can read a reply mixed with other startup bytes.
func parseCursorReport(buf []byte) (cols, rows int, ok bool) {
	for i := 0; i+4 < len(buf); i++ {
		if buf[i] != 0x1B || buf[i+1] != '[' {
			continue
		}
		j, r := i+2, 0
		for j < len(buf) && buf[j] >= '0' && buf[j] <= '9' {
			r = r*10 + int(buf[j]-'0')
			j++
		}
		if r == 0 || j >= len(buf) || buf[j] != ';' {
			continue
		}
		j++
		c := 0
		for j < len(buf) && buf[j] >= '0' && buf[j] <= '9' {
			c = c*10 + int(buf[j]-'0')
			j++
		}
		if c == 0 || j >= len(buf) || buf[j] != 'R' {
			continue
		}
		if c < 20 || r < 8 || c > 1000 || r > 1000 {
			continue
		}
		return c, r, true
	}
	return 0, 0, false
}

// parseWindowReport scans for ESC [ 8 ; <rows> ; <cols> t (xterm window report).
func parseWindowReport(buf []byte) (cols, rows int, ok bool) {
	for i := 0; i+5 < len(buf); i++ {
		if buf[i] != 0x1B || buf[i+1] != '[' || buf[i+2] != '8' || buf[i+3] != ';' {
			continue
		}
		j, r := i+4, 0
		for j < len(buf) && buf[j] >= '0' && buf[j] <= '9' {
			r = r*10 + int(buf[j]-'0')
			j++
		}
		if r == 0 || j >= len(buf) || buf[j] != ';' {
			continue
		}
		j++
		c := 0
		for j < len(buf) && buf[j] >= '0' && buf[j] <= '9' {
			c = c*10 + int(buf[j]-'0')
			j++
		}
		if c == 0 || j >= len(buf) || buf[j] != 't' {
			continue
		}
		if c < 20 || r < 8 || c > 1000 || r > 1000 {
			continue
		}
		return c, r, true
	}
	return 0, 0, false
}
