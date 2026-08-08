package main

import (
	"io"
	"testing"
	"time"
)

// The bug this guards: the token ceiling used to be a flat 200ms of link
// (rateEst*0.2). Whenever a frame cost more than that — which is most frames
// on any link slow enough to need governing — tokens could never reach
// lastFrameBytes, so videoBudgetOK never returned true and the picture froze
// for the entire session. Production telemetry caught it as governed sessions
// pinned at 0.0 fps (116x65 for 871 consecutive samples).
func TestGateAdmitsAFrameOnSlowLinks(t *testing.T) {
	// 5 KB/s link, 50 KB frame: a frame is 10 seconds of link, far past the
	// old 200ms ceiling of 1000 bytes.
	const rateEst, frameBytes = 5000.0, 50000.0

	if got := clampTokens(1e9, rateEst, frameBytes); got < frameBytes {
		t.Fatalf("ceiling %v is below one frame (%v): the gate can never open", got, frameBytes)
	}
	// Saturating the budget must actually open the gate.
	tokens := clampTokens(1e9, rateEst, frameBytes)
	if !videoBudgetOK(rateEst, tokens, frameBytes) {
		t.Fatal("budget saturated but the gate stayed shut")
	}
}

// Accumulating from empty must reach the gate in bounded time rather than
// asymptoting below it.
func TestGateReachableByAccumulation(t *testing.T) {
	const rateEst, frameBytes = 5000.0, 50000.0
	tokens := 0.0
	opened := false
	// 60 one-second refills at the governor's 85% duty cycle.
	for i := 0; i < 60; i++ {
		tokens += rateEst * 0.85
		tokens = clampTokens(tokens, rateEst, frameBytes)
		if videoBudgetOK(rateEst, tokens, frameBytes) {
			opened = true
			break
		}
	}
	if !opened {
		t.Fatal("60s of refill never opened the gate; video would be frozen")
	}
}

// Audio spends tokens ungated. That must not drive the budget so far negative
// that video stays dead long after the link recovers.
func TestAudioOverspendRecoveryIsBounded(t *testing.T) {
	const rateEst, frameBytes = 5000.0, 8000.0
	tokens := 0.0
	// A minute of audio far heavier than the link.
	for i := 0; i < 60; i++ {
		tokens -= 60000
		tokens = clampTokens(tokens, rateEst, frameBytes)
	}
	// Recovery must not take longer than the burst window it was floored at.
	steps := 0
	for !videoBudgetOK(rateEst, tokens, frameBytes) {
		tokens += rateEst * 0.85
		tokens = clampTokens(tokens, rateEst, frameBytes)
		steps++
		if steps > 300 {
			t.Fatal("video never recovered after audio overspend")
		}
	}
}

// An ungoverned link (never been the constraint) must stay wide open.
func TestUngovernedLinkIsNotGated(t *testing.T) {
	if !videoBudgetOK(0, -1e9, 50000) {
		t.Fatal("rateEst==0 must mean no governing at all")
	}
}

// ---- 3dBBS detection --------------------------------------------------------

func TestDetect3DBBSHandheld(t *testing.T) {
	ft := newFakeTerm(t)
	go func() {
		buf := make([]byte, len(featureProbe)+len(query3DS))
		io.ReadFull(ft.termOut, buf)
		if string(buf) != featureProbe+query3DS {
			t.Errorf("probes = %q, want %q", buf, featureProbe+query3DS)
		}
		// A 3dBBS client answers both, feature reply first (FIFO worker).
		ft.termIn.WriteString("\x1b[=7;100;1n\x1b_3DS:Ver;0;3\x1b\\")
	}()
	s := newAudioSession(defaultAudioConfig())
	s.detect(ft.doorIn, ft.doorOut, 2*time.Second)
	if !s.enabled {
		t.Fatal("handheld supports libsndfile; audio should be enabled")
	}
	if !s.is3DBBS {
		t.Fatal("3DS:Ver reply was not recognised as a handheld")
	}
}

// A desktop SyncTERM answers the feature probe and ignores the 3DS one. It
// must not be misread as a handheld, and must not wait out the full timeout.
func TestDetectDesktopIsNotHandheld(t *testing.T) {
	ft := newFakeTerm(t)
	go func() {
		buf := make([]byte, len(featureProbe)+len(query3DS))
		io.ReadFull(ft.termOut, buf)
		ft.termIn.WriteString("\x1b[=7;100;1n")
	}()
	s := newAudioSession(defaultAudioConfig())
	start := time.Now()
	s.detect(ft.doorIn, ft.doorOut, 5*time.Second)
	elapsed := time.Since(start)
	if !s.enabled {
		t.Fatal("desktop replied available; audio should be enabled")
	}
	if s.is3DBBS {
		t.Fatal("desktop terminal misdetected as a 3dBBS handheld")
	}
	// Bounded by the grace window, not the caller's timeout.
	if elapsed > 2*time.Second {
		t.Fatalf("desktop probe took %v; should return ~%v after the feature reply",
			elapsed, query3DSGrace)
	}
}

func TestParse3DSVerToleratesNoise(t *testing.T) {
	if !parse3DSVer([]byte("junk\x1b[74;162R\x1b_3DS:Ver;0;3\x1b\\more")) {
		t.Fatal("version reply mixed with CPR and typing was not found")
	}
	if parse3DSVer([]byte("\x1b[=7;100;1n")) {
		t.Fatal("a plain feature reply must not read as a handheld")
	}
}

// ---- 8-bit audio downconversion ---------------------------------------------

func TestEncodeWAV8HalvesBytesAndBiasesCorrectly(t *testing.T) {
	// S16LE samples: 0, +32767, -32768, -256.
	pcm := []byte{0x00, 0x00, 0xFF, 0x7F, 0x00, 0x80, 0x00, 0xFF}
	w := encodeWAV8(pcm, 22050, 1)
	if string(w[0:4]) != "RIFF" || string(w[8:12]) != "WAVE" || string(w[36:40]) != "data" {
		t.Fatalf("bad header: %q", w[:44])
	}
	if bits := w[34]; bits != 8 {
		t.Fatalf("bits per sample = %d, want 8", bits)
	}
	body := w[44:]
	if len(body) != len(pcm)/2 {
		t.Fatalf("payload = %d bytes, want %d (half)", len(body), len(pcm)/2)
	}
	// WAV 8-bit is UNSIGNED: silence is 128, full scale spans 0..255.
	want := []byte{128, 255, 0, 127}
	for i := range want {
		if body[i] != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, body[i], want[i])
		}
	}
}

// The duration a clip represents must be identical to the 16-bit path — the
// ledger bills chunks by declared duration, so a rate or frame-count change
// here would desync the audio clock.
func TestEncodeWAV8PreservesDuration(t *testing.T) {
	pcm := make([]byte, 22050*2) // 1s of mono s16 at 22050
	w16 := encodeWAV(pcm, 22050, 1)
	w8 := encodeWAV8(pcm, 22050, 1)

	rate16 := uint32(w16[24]) | uint32(w16[25])<<8 | uint32(w16[26])<<16 | uint32(w16[27])<<24
	rate8 := uint32(w8[24]) | uint32(w8[25])<<8 | uint32(w8[26])<<16 | uint32(w8[27])<<24
	if rate16 != rate8 {
		t.Fatalf("sample rate changed: %d -> %d", rate16, rate8)
	}
	frames16 := (len(w16) - 44) / 2
	frames8 := len(w8) - 44
	if frames16 != frames8 {
		t.Fatalf("frame count changed: %d -> %d", frames16, frames8)
	}
	if len(w8) >= len(w16) {
		t.Fatalf("8-bit clip (%d) is not smaller than 16-bit (%d)", len(w8), len(w16))
	}
}
