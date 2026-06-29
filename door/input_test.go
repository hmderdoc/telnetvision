package main

import "testing"

// feedAll runs a byte string through a fresh keyParser and returns every
// non-keyNone event it produced, plus the parser (to inspect residual state).
func feedAll(s string) (events []keyEvent, sizes [][2]int, kp keyParser) {
	for i := 0; i < len(s); i++ {
		ev, cols, rows := kp.feed(s[i])
		if ev != keyNone {
			events = append(events, ev)
			sizes = append(sizes, [2]int{cols, rows})
		}
	}
	return events, sizes, kp
}

func TestParseCPR(t *testing.T) {
	for _, tc := range []struct {
		params             string
		wantCols, wantRows int
		ok                 bool
	}{
		{"36;120", 120, 36, true}, // rows;cols
		{"25;80", 80, 25, true},
		{"2;5", 0, 0, false},      // both below the floor → rejected
		{"7;200", 0, 0, false},    // rows < 8 → rejected
		{"24;19", 0, 0, false},    // cols < 20 → rejected
		{"1001;500", 0, 0, false}, // rows > 1000 → rejected
		{"40", 0, 0, false},       // missing field
		{"a;b", 0, 0, false},      // non-numeric
		{"", 0, 0, false},
	} {
		cols, rows, ok := parseCPR([]byte(tc.params))
		if ok != tc.ok || (ok && (cols != tc.wantCols || rows != tc.wantRows)) {
			t.Errorf("parseCPR(%q) = (%d,%d,%v), want (%d,%d,%v)",
				tc.params, cols, rows, ok, tc.wantCols, tc.wantRows, tc.ok)
		}
	}
}

// The startup probe scans a raw buffer (possibly with surrounding noise) for the
// cursor / window report, unlike the live keyParser path.
func TestParseReportScanning(t *testing.T) {
	// CPR embedded in noise: leading keystroke, trailing junk.
	if c, r, ok := parseCursorReport([]byte("x\x1b[36;120Ry")); !ok || c != 120 || r != 36 {
		t.Errorf("parseCursorReport = %d,%d,%v, want 120,36,true", c, r, ok)
	}
	// Split-then-complete: a partial report shouldn't match until the final byte.
	if _, _, ok := parseCursorReport([]byte("\x1b[36;120")); ok {
		t.Error("parseCursorReport matched an incomplete report")
	}
	// Out-of-range rejected.
	if _, _, ok := parseCursorReport([]byte("\x1b[2;5R")); ok {
		t.Error("parseCursorReport accepted an out-of-range size")
	}
	// xterm window report ESC[8;rows;cols;t.
	if c, r, ok := parseWindowReport([]byte("\x1b[8;40;132t")); !ok || c != 132 || r != 40 {
		t.Errorf("parseWindowReport = %d,%d,%v, want 132,40,true", c, r, ok)
	}
	if _, _, ok := parseWindowReport([]byte("\x1b[36;120R")); ok {
		t.Error("parseWindowReport matched a cursor report")
	}
}

// A CPR reply must NOT be read as a quit even though it starts with ESC, and it
// must surface the reported size — the central bug this change fixes.
func TestKeyParser_CPRDoesNotQuit(t *testing.T) {
	events, sizes, kp := feedAll("\x1b[36;120R")
	if len(events) != 1 || events[0] != keyResize {
		t.Fatalf("events = %v, want exactly one keyResize", events)
	}
	if sizes[0] != [2]int{120, 36} {
		t.Errorf("size = %v, want [120 36]", sizes[0])
	}
	if kp.awaitingEscape() {
		t.Error("parser left awaiting ESC after a complete CPR")
	}
}

// An out-of-range report is parsed (so it doesn't desync the stream) but yields
// no resize event.
func TestKeyParser_CPRRejected(t *testing.T) {
	events, _, _ := feedAll("\x1b[2;5R")
	if len(events) != 0 {
		t.Errorf("events = %v, want none (report rejected)", events)
	}
}

func TestKeyParser_QuitKeys(t *testing.T) {
	for _, k := range []string{"q", "Q", "x", "X", "\r", "\n", "\x03"} {
		events, _, _ := feedAll(k)
		if len(events) != 1 || events[0] != keyQuit {
			t.Errorf("feed(%q) = %v, want one keyQuit", k, events)
		}
	}
}

// A lone ESC byte leaves the parser pending (the goroutine's timeout, not feed,
// decides it's a quit); an ESC followed by a non-sequence byte quits.
func TestKeyParser_EscHandling(t *testing.T) {
	if events, _, kp := feedAll("\x1b"); len(events) != 0 || !kp.awaitingEscape() {
		t.Errorf("lone ESC: events=%v awaiting=%v, want none/true", events, kp.awaitingEscape())
	}
	if events, _, _ := feedAll("\x1bZ"); len(events) != 1 || events[0] != keyQuit {
		t.Errorf("ESC+Z: events=%v, want one keyQuit", events)
	}
}

// Telnet IAC negotiation must be swallowed, not parsed as keys — in particular
// IAC DO SUPPRESS-GO-AHEAD (FF FD 03) must not trip the Ctrl-C (0x03) quit.
func TestKeyParser_IACSwallowed(t *testing.T) {
	// IAC DO SUPPRESS-GO-AHEAD, then IAC WILL ECHO, then a real 'Q'.
	events, _, _ := feedAll("\xff\xfd\x03\xff\xfb\x01Q")
	if len(events) != 1 || events[0] != keyQuit {
		t.Errorf("events = %v, want one keyQuit (from the trailing Q only)", events)
	}
	// IAC SB … IAC SE subnegotiation, including an escaped 0xFF, then 'q'.
	events, _, _ = feedAll("\xff\xfa\x18\x00\xff\xff\xff\xf0q")
	if len(events) != 1 || events[0] != keyQuit {
		t.Errorf("subneg: events = %v, want one keyQuit", events)
	}
}
