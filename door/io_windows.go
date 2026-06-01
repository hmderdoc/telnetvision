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

import "os"

func setNonblock(f *os.File) {}
func setBlock(f *os.File)    {}

// writeNB writes (blocking) to f; never reports a would-block.
func writeNB(f *os.File, b []byte) (int, error) {
	return f.Write(b)
}

// readInput reads (blocking) from f; again is always false on Windows.
func readInput(f *os.File, b []byte) (n int, again bool, err error) {
	n, err = f.Read(b)
	return n, false, err
}
