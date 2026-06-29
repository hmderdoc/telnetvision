//go:build windows

// Windows fallback. Console/pipe/socket writes don't surface EAGAIN, so the
// latency pacer degrades to blocking writes (one frame at a time) — it still
// drops to the latest frame between writes, just without the tight bound.
//
// For DOOR32.SYS mode, the inherited SOCKET handle is wrapped via os.NewFile;
// ReadFile/WriteFile work on a SOCKET as long as it wasn't created with
// WSA_FLAG_OVERLAPPED (the default for plain accept(), which is what
// RA-lineage BBSes use).
package main

import (
	"os"
	"time"
)

func setNonblock(f *os.File) {}
func setBlock(f *os.File)    {}

// readTimeout is a no-op on Windows: there's no reliably cancellable read on the
// *os.File door path, so the synchronous startup probe is skipped (the async
// reader still tracks size via live CPR reports). Returning (0, nil) immediately
// guarantees the probe loop never hangs on a silent terminal.
func readTimeout(f *os.File, p []byte, d time.Duration) (int, error) { return 0, nil }

// localTermSize isn't wired up on Windows (the console-info path needs
// golang.org/x/sys/windows, which the door otherwise avoids). Callers fall back
// to the ANSI probe / -termcols/-termrows / the 80x25 default.
func localTermSize(f *os.File) (cols, rows int, ok bool) { return 0, 0, false }

// writeNB writes (blocking) to f; never reports a would-block.
func writeNB(f *os.File, b []byte) (int, error) {
	return f.Write(b)
}

// readInput reads (blocking) from f; again is always false on Windows.
func readInput(f *os.File, b []byte) (n int, again bool, err error) {
	n, err = f.Read(b)
	return n, false, err
}
