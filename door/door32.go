// DOOR32.SYS parser. DOOR32.SYS is the dropfile RA-family BBSes (RemoteAccess,
// EleBBS, Mystic, MagickaBBS, ...) write when they launch an external program;
// when the caller is on telnet (line 1 == 2) line 2 carries the already-open
// socket handle the BBS inherited to us, so the door can read/write the
// caller's connection directly without any stdio-bridge plumbing.
//
// Spec:
//   1  Comm type  (0=local, 1=serial, 2=telnet socket, 3=other)
//   2  Comm/socket handle (integer)
//   3  Baud rate (0 for telnet/local)
//   4  BBSID
//   5  User record position
//   6  Real name
//   7  Handle/alias
//   8  Security level
//   9  Time left (minutes)
//   10 Emulation (0=ASCII, 1=ANSI, 2=AVATAR, 3=RIP, 4=MaxGraphics)
//   11 Node number
package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

const (
	door32CommLocal  = 0
	door32CommSerial = 1
	door32CommTelnet = 2
	door32CommOther  = 3
)

type door32Info struct {
	Comm   int    // line 1
	Handle uint64 // line 2 (socket handle when Comm=2)
	BBSID  string // line 4 (informational)
	User   string // line 7 (informational)
	Node   int    // line 11 (informational)
}

// readDoor32 parses a DOOR32.SYS dropfile. Missing file returns (nil, nil) so
// callers can fall back to stdio. A present-but-malformed file is an error —
// we don't want to silently mask a sysop's misconfiguration.
func readDoor32(path string) (*door32Info, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		lines = append(lines, strings.TrimSpace(sc.Text()))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("DOOR32.SYS: %d lines, need at least 2", len(lines))
	}
	comm, err := strconv.Atoi(lines[0])
	if err != nil {
		return nil, fmt.Errorf("DOOR32.SYS line 1 (comm type): %v", err)
	}
	handle, err := strconv.ParseUint(lines[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("DOOR32.SYS line 2 (handle): %v", err)
	}
	info := &door32Info{Comm: comm, Handle: handle}
	if len(lines) >= 4 {
		info.BBSID = lines[3]
	}
	if len(lines) >= 7 {
		info.User = lines[6]
	}
	if len(lines) >= 11 {
		if n, err := strconv.Atoi(lines[10]); err == nil {
			info.Node = n
		}
	}
	return info, nil
}
