//go:build unix

// Non-blocking stdin/stdout for the latency pacer (Linux, macOS, *BSD).
package main

import "syscall"

func setNonblock(fd int) { _ = syscall.SetNonblock(fd, true) }
func setBlock(fd int)    { _ = syscall.SetNonblock(fd, false) }

// writeNB writes without blocking. A would-block is reported as no error with
// the buffer not fully consumed, so the caller just retries on the next tick.
func writeNB(fd int, b []byte) (int, error) {
	n, err := syscall.Write(fd, b)
	if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
		err = nil
	}
	if n < 0 {
		n = 0
	}
	return n, err
}

// readStdin reads from fd 0; again=true means "no input right now".
func readStdin(b []byte) (n int, again bool, err error) {
	n, err = syscall.Read(0, b)
	if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
		return 0, true, nil
	}
	return n, false, err
}
