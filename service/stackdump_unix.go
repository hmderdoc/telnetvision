//go:build unix

package main

import (
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
)

// startStackDumper installs a SIGUSR1 handler that prints every goroutine's
// stack to stderr. Run `kill -USR1 <pid>` against a wedged service to see
// which goroutine is stuck where (lock, syscall, channel send/recv).
func startStackDumper() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGUSR1)
	go func() {
		buf := make([]byte, 1<<20)
		for range sigs {
			n := runtime.Stack(buf, true)
			log.Printf("===== SIGUSR1 stack dump (%d goroutines) =====\n%s\n===== end stack dump =====",
				runtime.NumGoroutine(), buf[:n])
		}
	}()
}
