//go:build windows

// Windows fallback. Console/pipe writes don't surface EAGAIN, so the latency
// pacer degrades to blocking writes (one frame at a time) — it still drops to
// the latest frame between writes, just without the tight non-blocking bound.
package main

import "os"

func setNonblock(fd int) {}
func setBlock(fd int)    {}

// writeNB writes (blocking) to stdout; never reports a would-block.
func writeNB(fd int, b []byte) (int, error) {
	return os.Stdout.Write(b)
}

// readStdin reads (blocking) from stdin; again is always false on Windows.
func readStdin(b []byte) (n int, again bool, err error) {
	n, err = os.Stdin.Read(b)
	return n, false, err
}
