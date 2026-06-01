//go:build unix

// Non-blocking I/O against an arbitrary *os.File — stdio in normal mode, the
// inherited DOOR32.SYS socket in EleBBS-style mode.
package main

import (
	"os"
	"syscall"
)

func setNonblock(f *os.File) { _ = syscall.SetNonblock(int(f.Fd()), true) }
func setBlock(f *os.File)    { _ = syscall.SetNonblock(int(f.Fd()), false) }

// writeNB writes without blocking. A would-block is reported as no error with
// the buffer not fully consumed, so the caller retries on the next tick.
func writeNB(f *os.File, b []byte) (int, error) {
	n, err := syscall.Write(int(f.Fd()), b)
	if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
		err = nil
	}
	if n < 0 {
		n = 0
	}
	return n, err
}

// readInput reads from f; again=true means "no input right now".
func readInput(f *os.File, b []byte) (n int, again bool, err error) {
	n, err = syscall.Read(int(f.Fd()), b)
	if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
		return 0, true, nil
	}
	return n, false, err
}
