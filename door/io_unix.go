//go:build unix

// Non-blocking I/O against an arbitrary *os.File — stdio in normal mode, the
// inherited DOOR32.SYS socket in EleBBS-style mode.
package main

import (
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

func setNonblock(f *os.File) { _ = syscall.SetNonblock(int(f.Fd()), true) }
func setBlock(f *os.File)    { _ = syscall.SetNonblock(int(f.Fd()), false) }

// readTimeout reads whatever is available within d, returning (0, nil) on
// timeout. It drives the fd non-blocking at the syscall level — os.File read
// deadlines aren't reliable on an inherited socket — and restores blocking mode.
// Used only by the startup size probe, before the reader goroutine starts.
func readTimeout(f *os.File, p []byte, d time.Duration) (int, error) {
	fd := int(f.Fd())
	syscall.SetNonblock(fd, true)
	defer syscall.SetNonblock(fd, false)
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		n, err := syscall.Read(fd, p)
		if n > 0 {
			return n, nil
		}
		if err == syscall.EAGAIN || err == syscall.EWOULDBLOCK {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, io.EOF
		}
	}
	return 0, nil
}

// localTermSize reads the terminal dimensions from the kernel via TIOCGWINSZ.
// Works only for a real local tty (stdio mode); through an inherited door socket
// the ioctl fails and the caller falls back to the ANSI probe.
func localTermSize(f *os.File) (cols, rows int, ok bool) {
	type winsize struct{ Row, Col, X, Y uint16 }
	var ws winsize
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(),
		uintptr(syscall.TIOCGWINSZ), uintptr(unsafe.Pointer(&ws)))
	if e != 0 || ws.Col == 0 || ws.Row == 0 {
		return 0, 0, false
	}
	return int(ws.Col), int(ws.Row), true
}

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
