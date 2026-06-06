//go:build windows

package main

// startStackDumper is a no-op on Windows: SIGUSR1 isn't a Win32 concept and
// the service isn't a Windows deployment target. The CI matrix still builds
// it for completeness, but the SIGUSR1 stack-dump diagnostic only fires on
// Unix targets (see stackdump_unix.go).
func startStackDumper() {}
