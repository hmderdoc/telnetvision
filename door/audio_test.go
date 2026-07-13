package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ---- fake terminal / helpers --------------------------------------------------

// apcRe patterns: every APC body the door may emit, per the cterm Audio APC
// grammar (C;S is semicolon-separated; Queue never on channels 0/1).
var apcPatterns = []*regexp.Regexp{
	regexp.MustCompile(`^SyncTERM:C;S;tv_\d+\.(wav|flac);[A-Za-z0-9+/]*={0,2}$`),
	regexp.MustCompile(`^SyncTERM:A;Load;S=\d+;tv_\d+\.(wav|flac)$`),
	regexp.MustCompile(`^SyncTERM:A;Queue;C=\d+;S=\d+$`),
	regexp.MustCompile(`^SyncTERM:A;Update;C=\d+$`),
	// The dB suffix is mandatory: a bare number is parsed as 0..100 percent
	// and a negative one clamps to the -60dB floor (i.e. silence).
	regexp.MustCompile(`^SyncTERM:A;Volume;C=\d+;V=-?\d+dB$`),
	regexp.MustCompile(`^SyncTERM:A;Flush;C=\d+(;O=\d+)?$`),
	regexp.MustCompile(`^SyncTERM:Q;libsndfile$`),
}

// splitAPCs extracts every ESC _ ... ESC \ body from a byte stream that may
// also contain non-APC output (DSR polls, frame paints).
func splitAPCs(t *testing.T, out []byte) []string {
	t.Helper()
	var bodies []string
	for {
		i := bytes.Index(out, []byte("\x1b_"))
		if i < 0 {
			return bodies
		}
		j := bytes.Index(out[i:], []byte("\x1b\\"))
		if j < 0 {
			t.Fatalf("unterminated APC at offset %d: %q", i, out[i:])
		}
		bodies = append(bodies, string(out[i+2:i+j]))
		out = out[i+j+2:]
	}
}

// assertValidAPCs checks every APC in the stream against the spec grammar and
// that no Queue ever targets the cterm-owned channels 0/1.
func assertValidAPCs(t *testing.T, out []byte) []string {
	t.Helper()
	bodies := splitAPCs(t, out)
	queueRe := regexp.MustCompile(`^SyncTERM:A;Queue;C=(\d+);S=`)
	for _, b := range bodies {
		ok := false
		for _, re := range apcPatterns {
			if re.MatchString(b) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("APC does not match any spec grammar: %q", b)
		}
		if m := queueRe.FindStringSubmatch(b); m != nil {
			ch, _ := strconv.Atoi(m[1])
			if ch < audioChanMin || ch > audioChanMax {
				t.Errorf("Queue on forbidden channel %d: %q", ch, b)
			}
		}
	}
	return bodies
}

// testMsg builds a wire AUDIO payload the way wire.py's audio_payload does.
func testMsg(codec byte, channels int, rate, seq int, pts int64, durMs int, payload []byte) []byte {
	p := []byte{msgAudio, codec, byte(channels)}
	p = binary.BigEndian.AppendUint32(p, uint32(rate))
	p = binary.BigEndian.AppendUint32(p, uint32(seq))
	p = binary.BigEndian.AppendUint64(p, uint64(pts))
	p = binary.BigEndian.AppendUint32(p, uint32(durMs))
	return append(p, payload...)
}

// pcmChunk is durMs of silence as raw S16LE at the given rate/channels.
func pcmChunk(durMs, rate, channels int) []byte {
	return make([]byte, rate*channels*2*durMs/1000)
}

func enabledSession(cfg audioConfig) *audioSession {
	cfg.Mode = "on"   // skip the probe; tests drive the ledger directly
	cfg.Prebuffer = 1 // steady-state behavior; cushion tests set their own
	s := newAudioSession(cfg)
	s.enabled = true
	return s
}

func cushionSession(prebuffer int) *audioSession {
	cfg := defaultAudioConfig()
	cfg.Mode = "on"
	cfg.Prebuffer = prebuffer
	s := newAudioSession(cfg)
	s.enabled = true
	return s
}

// ---- feature detect ------------------------------------------------------------

// fakeTerm wires detect()'s in/out *os.File ends to pipes so a test can play
// the terminal: read the probe from doorOut, write replies into doorIn.
type fakeTerm struct {
	doorIn, doorOut *os.File // the ends handed to the door
	termIn, termOut *os.File // the ends the fake terminal drives
}

func newFakeTerm(t *testing.T) *fakeTerm {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	ft := &fakeTerm{doorIn: inR, doorOut: outW, termIn: inW, termOut: outR}
	t.Cleanup(func() {
		inR.Close()
		inW.Close()
		outR.Close()
		outW.Close()
	})
	return ft
}

func TestDetectFeaturePresent(t *testing.T) {
	ft := newFakeTerm(t)
	go func() { // echo the libsndfile-available reply once the probe arrives
		buf := make([]byte, len(featureProbe))
		io.ReadFull(ft.termOut, buf)
		if string(buf) != featureProbe {
			t.Errorf("probe = %q, want %q", buf, featureProbe)
		}
		ft.termIn.WriteString("\x1b[=7;100;1n")
	}()
	s := newAudioSession(defaultAudioConfig())
	s.detect(ft.doorIn, ft.doorOut, time.Second)
	if !s.enabled {
		t.Fatal("terminal replied available; session should be enabled")
	}
}

func TestDetectFeatureAbsent(t *testing.T) {
	ft := newFakeTerm(t)
	go func() {
		buf := make([]byte, len(featureProbe))
		io.ReadFull(ft.termOut, buf)
		ft.termIn.WriteString("\x1b[=7;100;0n") // built without libsndfile
	}()
	s := newAudioSession(defaultAudioConfig())
	s.detect(ft.doorIn, ft.doorOut, time.Second)
	if s.enabled {
		t.Fatal("terminal replied unavailable; session must stay disabled")
	}
}

func TestDetectTimeoutOnSilentTerminal(t *testing.T) {
	ft := newFakeTerm(t) // an old SyncTERM: consumes the APC, never replies
	start := time.Now()
	s := newAudioSession(defaultAudioConfig())
	s.detect(ft.doorIn, ft.doorOut, 150*time.Millisecond)
	if s.enabled {
		t.Fatal("no reply; session must be disabled")
	}
	if e := time.Since(start); e > time.Second {
		t.Fatalf("detect took %v; must respect its timeout", e)
	}
}

func TestDetectModeOffNeverProbes(t *testing.T) {
	ft := newFakeTerm(t)
	cfg := defaultAudioConfig()
	cfg.Mode = "off"
	s := newAudioSession(cfg)
	s.detect(ft.doorIn, ft.doorOut, time.Second)
	if s.enabled {
		t.Fatal("audio=off must disable audio")
	}
	ft.doorOut.Close()
	if n, _ := io.ReadAll(ft.termOut); len(n) != 0 {
		t.Fatalf("audio=off must not write a probe; got %q", n)
	}
}

// ---- reply parsing ---------------------------------------------------------------

func TestParseDSRPairs(t *testing.T) {
	pairs, ok := parseDSRPairs([]byte("=7;2;0"))
	if !ok || len(pairs) != 1 || pairs[0] != [2]int{2, 0} {
		t.Fatalf("got %v ok=%v", pairs, ok)
	}
	pairs, ok = parseDSRPairs([]byte("=7;100;1;2;0"))
	if !ok || len(pairs) != 2 || pairs[0] != [2]int{100, 1} || pairs[1] != [2]int{2, 0} {
		t.Fatalf("got %v ok=%v", pairs, ok)
	}
	if _, ok = parseDSRPairs([]byte("6")); ok {
		t.Fatal("plain CPR params must not parse as an audio DSR")
	}
	if _, ok = parseDSRPairs([]byte("=8;1;1")); ok {
		t.Fatal("non-audio private DSR must not parse")
	}
}

func TestParseFeatureReplyTolatesNoise(t *testing.T) {
	buf := []byte("garbage\x1b[12;80R more \x1b[=7;100;1n tail")
	avail, found := parseFeatureReply(buf, audioFeatureID)
	if !found || !avail {
		t.Fatalf("found=%v avail=%v", found, avail)
	}
}

func TestKeyParserRoutesAudioDSR(t *testing.T) {
	var kp keyParser
	input := "\x1b[=7;2;0n"
	var got keyEvent
	for i := 0; i < len(input); i++ {
		ev, _, _ := kp.feed(input[i])
		if ev != keyNone {
			got = ev
		}
	}
	if got != keyAudioDSR {
		t.Fatalf("event = %v, want keyAudioDSR", got)
	}
	pairs, ok := parseDSRPairs(kp.params)
	if !ok || len(pairs) != 1 || pairs[0] != [2]int{2, 0} {
		t.Fatalf("params %q -> %v ok=%v", kp.params, pairs, ok)
	}
	// And a CPR still parses as a resize after the DSR.
	var cols, rows int
	for i := 0; i < len("\x1b[24;80R"); i++ {
		ev, c, r := kp.feed("\x1b[24;80R"[i])
		if ev == keyResize {
			cols, rows = c, r
		}
	}
	if cols != 80 || rows != 24 {
		t.Fatalf("CPR after DSR gave %dx%d", cols, rows)
	}
}

func TestKeyParserMuteAndMouseReports(t *testing.T) {
	var kp keyParser
	// A bare keypress toggles mute.
	if ev, _, _ := kp.feed('m'); ev != keyMute {
		t.Fatalf("'m' must be keyMute, got %v", ev)
	}
	if ev, _, _ := kp.feed('M'); ev != keyMute {
		t.Fatalf("'M' must be keyMute, got %v", ev)
	}
	// An SGR mouse report (ESC[<0;99;25M) must be swallowed whole — its
	// trailing M must NOT leak into ground state and toggle mute per click.
	for _, seq := range []string{"\x1b[<0;99;25M", "\x1b[<0;99;25m", "\x1b[?25h"} {
		for i := 0; i < len(seq); i++ {
			if ev, _, _ := kp.feed(seq[i]); ev != keyNone {
				t.Fatalf("%q byte %d produced event %v; control sequences must be inert", seq, i, ev)
			}
		}
	}
	// Still a working parser afterwards.
	if ev, _, _ := kp.feed('m'); ev != keyMute {
		t.Fatal("parser must return to ground after control sequences")
	}
}

// ---- wire parsing ----------------------------------------------------------------

func TestParseAudioMsgRoundTrip(t *testing.T) {
	payload := pcmChunk(80, 44100, 2)
	p := testMsg(codecWAV, 2, 44100, 7, 1234, 80, payload)
	m, ok := parseAudioMsg(p)
	if !ok {
		t.Fatal("parse failed")
	}
	if m.Codec != codecWAV || m.Channels != 2 || m.SampleRate != 44100 ||
		m.Seq != 7 || m.PtsMs != 1234 || m.DurMs != 80 || len(m.Payload) != len(payload) {
		t.Fatalf("bad fields: %+v", m)
	}
	if _, ok := parseAudioMsg([]byte{msgAudio, 0, 2}); ok {
		t.Fatal("truncated message must not parse")
	}
	if _, ok := parseAudioMsg(p[1:]); ok {
		t.Fatal("wrong type byte must not parse")
	}
}

func TestEncodeWAVHeader(t *testing.T) {
	pcm := []byte{1, 2, 3, 4}
	w := encodeWAV(pcm, 44100, 2)
	if string(w[0:4]) != "RIFF" || string(w[8:12]) != "WAVE" || string(w[36:40]) != "data" {
		t.Fatalf("bad header: %q", w[:44])
	}
	if binary.LittleEndian.Uint32(w[24:28]) != 44100 {
		t.Fatal("rate field wrong")
	}
	if binary.LittleEndian.Uint16(w[22:24]) != 2 {
		t.Fatal("channels field wrong")
	}
	if len(w) != 44+len(pcm) {
		t.Fatalf("len = %d", len(w))
	}
}

// ---- emission: slot pool + grammar ------------------------------------------------

func TestSlotRotationAndGrammar(t *testing.T) {
	cfg := defaultAudioConfig()
	cfg.SlotStart, cfg.SlotCount = 100, 3
	s := enabledSession(cfg)
	var out bytes.Buffer
	if err := s.start(&out); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1000, 0)
	for i := 0; i < 5; i++ {
		m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, i, int64(i*80), 80, pcmChunk(80, 44100, 2)))
		// Advance the clock chunk-by-chunk so backpressure never trips here.
		queued, err := s.handleChunk(&out, m, now.Add(time.Duration(i*80)*time.Millisecond))
		if err != nil || !queued {
			t.Fatalf("chunk %d: queued=%v err=%v", i, queued, err)
		}
	}
	bodies := assertValidAPCs(t, out.Bytes())

	var loads []string
	for _, b := range bodies {
		if strings.HasPrefix(b, "SyncTERM:A;Load;S=") {
			loads = append(loads, strings.TrimPrefix(strings.Split(b, ";")[2], "S="))
		}
	}
	want := []string{"100", "101", "102", "100", "101"} // pool of 3, rotating
	if fmt.Sprint(loads) != fmt.Sprint(want) {
		t.Fatalf("slot order %v, want %v", loads, want)
	}
	// Each triple must be contiguous and self-consistent: C;S names the file,
	// Load binds it to the slot, Queue plays that slot.
	for i := 0; i+2 < len(bodies); {
		if !strings.HasPrefix(bodies[i], "SyncTERM:C;S;") {
			i++
			continue
		}
		name := strings.Split(strings.TrimPrefix(bodies[i], "SyncTERM:C;S;"), ";")[0]
		if !strings.Contains(bodies[i+1], ";"+name) {
			t.Fatalf("Load %q does not reference stored file %q", bodies[i+1], name)
		}
		slot := strings.TrimPrefix(strings.Split(bodies[i+1], ";")[2], "S=")
		if !strings.HasSuffix(bodies[i+2], ";S="+slot) {
			t.Fatalf("Queue %q does not play loaded slot %s", bodies[i+2], slot)
		}
		i += 3
	}
}

func TestRawPCMWrappedAsWAV(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, 0, 0, 80, pcmChunk(80, 44100, 2)))
	s.handleChunk(&out, m, time.Unix(1000, 0))
	for _, b := range splitAPCs(t, out.Bytes()) {
		if strings.HasPrefix(b, "SyncTERM:C;S;") && !strings.Contains(b, ".wav;") {
			t.Fatalf("raw PCM must be cached as .wav: %q", b)
		}
	}
}

func TestFLACPassesThroughOpaque(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	fakeFlac := []byte("fLaC-not-really")
	m, _ := parseAudioMsg(testMsg(codecFLAC, 2, 44100, 0, 0, 80, fakeFlac))
	s.handleChunk(&out, m, time.Unix(1000, 0))
	bodies := splitAPCs(t, out.Bytes())
	found := false
	for _, b := range bodies {
		if strings.HasPrefix(b, "SyncTERM:C;S;tv_") && strings.Contains(b, ".flac;") {
			found = true
		}
	}
	if !found {
		t.Fatalf("FLAC chunk not cached as .flac: %v", bodies)
	}
}

// ---- backpressure ledger ----------------------------------------------------------

func TestLeadPileupTriggersResync(t *testing.T) {
	cfg := defaultAudioConfig()
	cfg.MaxQueued = 3
	s := enabledSession(cfg)
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	// A source clock faster than the wall: 80ms chunks arriving every 40ms.
	// The queue piles past the budget; the session must NOT drop the fresh
	// audio — it flushes the stale tail (skip to the present) and continues.
	// One cap cycle is ~6 chunks at this rate; 20 chunks must re-cap.
	for i := 0; i < 20; i++ {
		m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, i, int64(i*80), 80, pcmChunk(80, 44100, 2)))
		q, err := s.handleChunk(&out, m, now.Add(time.Duration(i*40)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if !q {
			t.Fatalf("chunk %d not queued — fresh audio must never be dropped", i)
		}
	}
	if s.resyncs < 2 {
		t.Fatalf("a persistently fast source must keep re-capping (resyncs=%d)", s.resyncs)
	}
	flushes := bytes.Count(out.Bytes(), []byte("A;Flush;C=2"))
	if flushes != s.resyncs {
		t.Fatalf("%d Flush commands for %d resyncs", flushes, s.resyncs)
	}
	// The lead stays BOUNDED (budget + one cap cycle of slop) instead of
	// growing by 40ms per chunk forever (20 chunks unchecked ≈ 800ms+).
	lastNow := now.Add(time.Duration(19*40) * time.Millisecond)
	if lead := s.leadMsLocked(lastNow); lead > int64(cfg.MaxQueued)*80+160 {
		t.Fatalf("lead %dms not capped by resync", lead)
	}
	assertValidAPCs(t, out.Bytes())
}

func TestBlindResyncCapsSilentTailGrowth(t *testing.T) {
	cfg := defaultAudioConfig()
	cfg.ResyncSecs = 2
	s := enabledSession(cfg)
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	// Steady realtime flow: chunks every 80ms, no drains, no primes — the
	// ledger looks perfect, but terminal-side clock drift could be growing a
	// tail. After ResyncSecs with no proof-of-empty, tick must reset it.
	for i := 0; i < 30; i++ {
		m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, i, int64(i*80), 80, pcmChunk(80, 44100, 2)))
		s.handleChunk(&out, m, now.Add(time.Duration(i*80)*time.Millisecond))
	}
	out.Reset()
	s.tick(&out, now.Add(2500*time.Millisecond))
	if s.resyncs != 1 || !bytes.Contains(out.Bytes(), []byte("A;Flush;C=2")) {
		t.Fatalf("2.5s of continuous playback must blind-resync once (resyncs=%d)", s.resyncs)
	}
	// The resync itself is proof-of-empty: no immediate re-trigger.
	out.Reset()
	s.tick(&out, now.Add(2600*time.Millisecond))
	if s.resyncs != 1 {
		t.Fatal("blind resync must not re-trigger within the interval")
	}
	// A trusted drain report also resets the timer.
	s.onDSR([][2]int{{2, 0}}, now.Add(4*time.Second))
	s.tick(&out, now.Add(5*time.Second))
	if s.resyncs != 1 {
		t.Fatal("a drain report proves empty; the blind timer must restart")
	}
	assertValidAPCs(t, out.Bytes())
}

func TestDrainDSRReanchorsLedger(t *testing.T) {
	cfg := defaultAudioConfig()
	s := enabledSession(cfg)
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, 0, 0, 80, pcmChunk(80, 44100, 2)))
	s.handleChunk(&out, m, now)
	if s.emittedMs == 0 {
		t.Fatal("precondition: ledger has queued audio")
	}
	// The terminal reports the channel drained — long after the last queue,
	// so it's trustworthy: the ledger re-anchors, and the resume (with its
	// silence prime) re-arms the Update one-shot itself.
	drainAt := now.Add(600 * time.Millisecond)
	s.onDSR([][2]int{{cfg.Channel, 0}}, drainAt)
	if s.emittedMs != 0 {
		t.Fatal("a trusted drain report must re-anchor the ledger")
	}
	out.Reset()
	m2, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, 1, 600, 80, pcmChunk(80, 44100, 2)))
	if q, _ := s.handleChunk(&out, m2, drainAt.Add(20*time.Millisecond)); !q {
		t.Fatal("after a drain report chunks must flow immediately")
	}
	if !bytes.Contains(out.Bytes(), []byte("A;Update;C=2")) {
		t.Fatalf("the resume must re-arm Update; got %q", out.Bytes())
	}
	out.Reset()
	s.tick(&out, drainAt.Add(30*time.Millisecond))
	if bytes.Contains(out.Bytes(), []byte("A;Update;C=2")) {
		t.Fatal("tick must not double-arm after the resume already re-armed")
	}
}

func TestTickPollsChannelState(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, 0, 0, 80, pcmChunk(80, 44100, 2)))
	s.handleChunk(&out, m, now)
	out.Reset()
	s.tick(&out, now.Add(time.Second))
	if !bytes.Contains(out.Bytes(), []byte("\x1b[=7;2n")) {
		t.Fatalf("expected a channel-state poll; got %q", out.Bytes())
	}
	out.Reset()
	s.tick(&out, now.Add(time.Second+50*time.Millisecond))
	if bytes.Contains(out.Bytes(), []byte("\x1b[=7;2n")) {
		t.Fatal("poll must be rate-limited to the 250ms interval")
	}
}

// ---- cushion (prebuffer) ----------------------------------------------------------

func mkChunk(seq int, pts int64) audioMsg {
	m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, seq, pts, 80, pcmChunk(80, 44100, 2)))
	return m
}

func TestCushionHoldsThenBursts(t *testing.T) {
	s := cushionSession(3)
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	for i := 0; i < 2; i++ {
		q, err := s.handleChunk(&out, mkChunk(i, int64(i*80)), now.Add(time.Duration(i*80)*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if q || out.Len() != 0 {
			t.Fatalf("chunk %d must be held while the cushion fills (queued=%v, %d bytes out)", i, q, out.Len())
		}
	}
	q, err := s.handleChunk(&out, mkChunk(2, 160), now.Add(160*time.Millisecond))
	if err != nil || !q {
		t.Fatalf("cushion full: burst expected (queued=%v err=%v)", q, err)
	}
	bodies := assertValidAPCs(t, out.Bytes())
	queues := 0
	for _, b := range bodies {
		if strings.HasPrefix(b, "SyncTERM:A;Queue;") {
			queues++
		}
	}
	if queues != 3 {
		t.Fatalf("burst queued %d clips, want the whole cushion (3)", queues)
	}
	// Ledger accounts the full burst: lead ≈ 3 chunks right after it.
	if lead := s.leadMsLocked(now.Add(160 * time.Millisecond)); lead != 240 {
		t.Fatalf("lead after burst = %dms, want 240", lead)
	}
}

func TestResumeAfterGapPrimesSilence(t *testing.T) {
	s := cushionSession(2)
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	s.handleChunk(&out, mkChunk(0, 0), now)
	s.handleChunk(&out, mkChunk(1, 80), now) // cushion full -> burst
	if q, _ := s.handleChunk(&out, mkChunk(2, 160), now.Add(160*time.Millisecond)); !q {
		t.Fatal("steady state: chunk must queue immediately")
	}
	// The stream goes quiet (upstream stall), then resumes. The resume must
	// go out IMMEDIATELY — no holding — led by a silent clip that rebuilds
	// the reserve, plus a re-arm of the drain one-shot.
	out.Reset()
	resumeAt := now.Add(800 * time.Millisecond)
	if q, _ := s.handleChunk(&out, mkChunk(3, 240), resumeAt); !q {
		t.Fatal("resume chunk must queue immediately, never be held")
	}
	bodies := assertValidAPCs(t, out.Bytes())
	stores, queues := 0, 0
	for _, b := range bodies {
		if strings.HasPrefix(b, "SyncTERM:C;S;") {
			stores++
		}
		if strings.HasPrefix(b, "SyncTERM:A;Queue;") {
			queues++
		}
	}
	if stores != 2 || queues != 2 {
		t.Fatalf("resume must emit silence + real chunk (2 triples), got %d stores %d queues: %v",
			stores, queues, bodies)
	}
	if !bytes.Contains(out.Bytes(), []byte("A;Update;C=2")) {
		t.Fatalf("resume must re-arm the drain one-shot; got %q", out.Bytes())
	}
	if s.primes != 1 {
		t.Fatalf("primes = %d, want 1", s.primes)
	}
	// The ledger re-anchored at the resume with the silent reserve in front.
	if lead := s.leadMsLocked(resumeAt); lead != 2*80+80 {
		t.Fatalf("lead after resume = %dms, want silence(160) + chunk(80)", lead)
	}
	// Steady flow continues without further primes.
	out.Reset()
	if q, _ := s.handleChunk(&out, mkChunk(4, 320), resumeAt.Add(80*time.Millisecond)); !q {
		t.Fatal("post-resume chunk must queue immediately")
	}
	if s.primes != 1 {
		t.Fatal("steady flow must not re-prime")
	}
}

// TestStaleDrainReportIgnored reproduces the field failure: a stopped-report
// arriving just after audio was queued is describing the pre-burst past (it
// spent an RTT in flight). Trusting it re-enters refill, the refill hold
// causes a REAL drain, and the loop self-sustains — heard as rhythmic chop,
// with lead pinned at 0 and video frozen by the A/V gate.
func TestStaleDrainReportIgnored(t *testing.T) {
	s := enabledSession(defaultAudioConfig()) // prebuffer 1: steady state
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	s.handleChunk(&out, mkChunk(0, 0), now)
	// A stopped-report lands 100ms after that queue: stale. No re-anchor.
	s.onDSR([][2]int{{2, 0}}, now.Add(100*time.Millisecond))
	if s.emittedMs == 0 {
		t.Fatal("stale drain report must not re-anchor the ledger")
	}
	// Steady flow continues: the next chunk queues immediately, unheld.
	out.Reset()
	if q, _ := s.handleChunk(&out, mkChunk(1, 80), now.Add(80*time.Millisecond)); !q || out.Len() == 0 {
		t.Fatal("steady state must continue after a stale report")
	}
	// But a report arriving long after the last queue is a genuine underrun:
	// the ledger re-anchors so the next resume silence-primes correctly.
	s.onDSR([][2]int{{2, 0}}, now.Add(80*time.Millisecond+drainReportLag+time.Millisecond))
	if s.emittedMs != 0 {
		t.Fatal("a genuine (aged) drain report must re-anchor the ledger")
	}
}

func TestStaleCushionFlushesOnTick(t *testing.T) {
	s := cushionSession(3)
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	s.handleChunk(&out, mkChunk(0, 0), now) // held: 1 of 3
	if out.Len() != 0 {
		t.Fatal("precondition: chunk held")
	}
	s.tick(&out, now.Add(100*time.Millisecond))
	if bytes.Contains(out.Bytes(), []byte("A;Queue")) {
		t.Fatal("cushion still fresh: tick must not flush it")
	}
	s.tick(&out, now.Add(500*time.Millisecond))
	if !bytes.Contains(out.Bytes(), []byte("A;Queue;C=2")) {
		t.Fatalf("stale cushion must flush on tick; got %q", out.Bytes())
	}
	assertValidAPCs(t, out.Bytes())
}

// ---- A/V sync gate ----------------------------------------------------------------

func TestVideoGate(t *testing.T) {
	cfg := defaultAudioConfig() // slack 120ms
	s := enabledSession(cfg)
	now := time.Unix(1000, 0)
	// Nothing queued yet: video must never be gated by silent audio.
	if g := s.videoGate(5000, now); g != gateRender {
		t.Fatalf("ungated render expected before priming, got %d", g)
	}
	// Queue one 80ms chunk with pts 1000..1080; immediately after, the
	// playing pts is ~1000 (lead ≈ 80ms).
	var out bytes.Buffer
	m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, 0, 1000, 80, pcmChunk(80, 44100, 2)))
	s.handleChunk(&out, m, now)
	if g := s.videoGate(1010, now); g != gateRender {
		t.Fatalf("frame within slack must render, got %d", g)
	}
	if g := s.videoGate(600, now); g != gateDrop {
		t.Fatalf("frame far behind audio must drop, got %d", g)
	}
	if g := s.videoGate(1500, now); g != gateHold {
		t.Fatalf("frame far ahead of audio must hold, got %d", g)
	}
	// A stale audio clock (no chunks for a while) releases the gate.
	if g := s.videoGate(600, now.Add(5*time.Second)); g != gateRender {
		t.Fatalf("stale audio clock must not gate video, got %d", g)
	}
}

// ---- mute -----------------------------------------------------------------------

func TestMuteFlushesDiscardsAndResumes(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	s.handleChunk(&out, mkChunk(0, 0), now)

	if muted := s.toggleMute(); !muted {
		t.Fatal("toggle must report muted")
	}
	out.Reset()
	s.tick(&out, now.Add(50*time.Millisecond))
	if !bytes.Contains(out.Bytes(), []byte("\x1b_SyncTERM:A;Flush;C=2\x1b\\")) {
		t.Fatalf("mute must flush the channel on the next tick; got %q", out.Bytes())
	}
	// While muted: chunks discarded, no polls/APCs emitted, video ungated.
	out.Reset()
	if q, _ := s.handleChunk(&out, mkChunk(1, 80), now.Add(100*time.Millisecond)); q || out.Len() != 0 {
		t.Fatal("muted session must discard chunks silently")
	}
	s.tick(&out, now.Add(2*time.Second))
	if out.Len() != 0 {
		t.Fatalf("muted session must stay silent on ticks; got %q", out.Bytes())
	}
	if g := s.videoGate(99999, now.Add(200*time.Millisecond)); g != gateRender {
		t.Fatal("muted session must never gate video")
	}
	// Unmute: the next chunk resumes and re-primes the cushion (a gap
	// necessarily elapsed while muted).
	if muted := s.toggleMute(); muted {
		t.Fatal("toggle must report unmuted")
	}
	out.Reset()
	resumeAt := now.Add(3 * time.Second)
	if q, _ := s.handleChunk(&out, mkChunk(2, 160), resumeAt); !q {
		t.Fatal("first chunk after unmute must queue")
	}
	if !bytes.Contains(out.Bytes(), []byte("A;Queue;C=2")) || s.primes != 1 {
		t.Fatalf("unmute must resume with the gap prime (primes=%d)", s.primes)
	}
	assertValidAPCs(t, out.Bytes())
}

func TestMuteIsNoopWhenAudioDisabled(t *testing.T) {
	s := newAudioSession(defaultAudioConfig()) // never enabled
	if s.toggleMute() {
		t.Fatal("mute must be a no-op without audio")
	}
}

// ---- session end --------------------------------------------------------------------

func TestStopFlushesImmediately(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	m, _ := parseAudioMsg(testMsg(codecRaw, 2, 44100, 0, 0, 80, pcmChunk(80, 44100, 2)))
	s.handleChunk(&out, m, time.Unix(1000, 0))
	out.Reset()
	if err := s.stop(&out); err != nil {
		t.Fatal(err)
	}
	// Plain immediate flush — no fade parameter, no carry-over risk.
	if !bytes.Contains(out.Bytes(), []byte("\x1b_SyncTERM:A;Flush;C=2\x1b\\")) {
		t.Fatalf("stop must flush the channel; got %q", out.Bytes())
	}
	assertValidAPCs(t, out.Bytes())
}

// TestHiddenTailTriggersResync reproduces the field pile-up: the ledger reads
// lead=0 (by our clock the queue should be dry) while the terminal's poll
// replies say "running" for many consecutive seconds — proof of a queue the
// ledger can't see (the caller's audio clock runs slower than ours). The
// evidence must convict within the window and trigger a resync.
func TestHiddenTailTriggersResync(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	s.handleChunk(&out, mkChunk(0, 0), now)

	// Ledger drains to 0 (wall advances past the queued audio), yet polls
	// keep reporting running.
	empty := now.Add(2 * time.Second)
	for i := 0; i <= 6; i++ {
		s.onDSR([][2]int{{2, 1}}, empty.Add(time.Duration(i)*time.Second))
	}
	if !s.needResync {
		t.Fatal("persistent running-while-empty must convict a hidden tail")
	}
	out.Reset()
	if err := s.tick(&out, empty.Add(7*time.Second)); err != nil {
		t.Fatal(err)
	}
	if s.resyncs != 1 || !bytes.Contains(out.Bytes(), []byte("A;Flush;C=2")) {
		t.Fatalf("tick must resync on hidden-tail evidence (resyncs=%d)", s.resyncs)
	}
	assertValidAPCs(t, out.Bytes())

	// The resync primed a cushion: lead>0 resets the evidence, and a
	// stopped report also clears it — no resync loops.
	if !s.runningEmptySince.IsZero() {
		t.Fatal("resync must reset the evidence window")
	}
	s.onDSR([][2]int{{2, 1}}, empty.Add(8*time.Second))
	s.onDSR([][2]int{{2, 0}}, empty.Add(9*time.Second))
	if !s.runningEmptySince.IsZero() || s.needResync {
		t.Fatal("a stopped report must clear the hidden-tail evidence")
	}
}

// TestRunningWithHealthyLeadIsNotEvidence: running reports are normal while
// the ledger has lead; they must not accumulate toward a resync.
func TestRunningWithHealthyLeadIsNotEvidence(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	now := time.Unix(1000, 0)
	// Keep lead > 0 by feeding chunks as we poll.
	for i := 0; i <= 8; i++ {
		at := now.Add(time.Duration(i*80) * time.Millisecond)
		s.handleChunk(&out, mkChunk(i, int64(i*80)), at)
		s.onDSR([][2]int{{2, 1}}, at)
	}
	if s.needResync || !s.runningEmptySince.IsZero() {
		t.Fatal("running with a healthy ledger lead is not hidden-tail evidence")
	}
}

func TestStartSetsVolumeInDecibels(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	if err := s.start(&out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte("A;Volume;C=2;V=-6dB")) {
		t.Fatalf("start must set volume with the dB suffix (bare numbers are percent"+
			" and negative percent = -60dB floor); got %q", out.Bytes())
	}
	assertValidAPCs(t, out.Bytes())
}

func TestStopIsSilentWhenNeverPrimed(t *testing.T) {
	s := enabledSession(defaultAudioConfig())
	var out bytes.Buffer
	s.stop(&out)
	if out.Len() != 0 {
		t.Fatalf("nothing queued: stop must write nothing, got %q", out.Bytes())
	}
}

// ---- config ---------------------------------------------------------------------------

func TestConfigClampsAndINIOverlay(t *testing.T) {
	ini := map[string]string{
		"audio":             "on",
		"audio_channel":     "5",
		"audio_slot_start":  "10",
		"audio_slot_count":  "20",
		"audio_max_queued":  "3",
		"audio_prebuffer":   "2",
		"sync_slack_ms":     "200",
		"audio_volume_db":   "-12",
		"audio_resync_secs": "30",
	}
	cfg := audioConfigFromINI(ini)
	want := audioConfig{Mode: "on", Channel: 5, SlotStart: 10, SlotCount: 20,
		MaxQueued: 3, Prebuffer: 2, SlackMs: 200, VolumeDB: -12, ResyncSecs: 30}
	if cfg != want {
		t.Fatalf("cfg = %+v, want %+v", cfg, want)
	}
	// The reserved channels and out-of-range slots must clamp.
	s := newAudioSession(audioConfig{Mode: "on", Channel: 0, SlotStart: 200, SlotCount: 100})
	if s.cfg.Channel < audioChanMin {
		t.Fatalf("channel %d not clamped off the reserved range", s.cfg.Channel)
	}
	if s.cfg.SlotStart+s.cfg.SlotCount > 256 {
		t.Fatalf("slot pool %d+%d exceeds the 256-slot cap", s.cfg.SlotStart, s.cfg.SlotCount)
	}
	if cfg2 := audioConfigFromINI(map[string]string{"audio": "banana"}); cfg2.Mode != "auto" {
		t.Fatalf("bad mode must keep the default, got %q", cfg2.Mode)
	}
}

// ---- frame segmentation (audio interleave points) -----------------------------------

func TestSplitANSIUnits(t *testing.T) {
	// A synthetic paint: escape sequences with glyphs, like the renderers emit.
	var frame bytes.Buffer
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&frame, "\x1b[38;2;%d;%d;%dm\xdf", i%256, (i*7)%256, (i*13)%256)
	}
	orig := frame.Bytes()
	units := splitANSIUnits(orig, 4096)
	if len(units) < 2 {
		t.Fatalf("a %dB frame must split into multiple units", len(orig))
	}
	if !bytes.Equal(bytes.Join(units, nil), orig) {
		t.Fatal("segmentation must be lossless")
	}
	for i, u := range units {
		if i > 0 && u[0] != 0x1B {
			t.Fatalf("unit %d starts mid-sequence (byte 0x%02x) — injecting audio here would corrupt the stream", i, u[0])
		}
		if i < len(units)-1 && len(u) < 4096 {
			t.Fatalf("unit %d is undersized (%dB)", i, len(u))
		}
	}
	// Small frames stay whole; ESC-free tails don't loop forever.
	if got := splitANSIUnits([]byte("small"), 4096); len(got) != 1 {
		t.Fatalf("small input must be one unit, got %d", len(got))
	}
	long := bytes.Repeat([]byte("x"), 10000)
	if got := splitANSIUnits(long, 4096); len(got) != 1 {
		t.Fatalf("ESC-free input can't be cut safely; must stay one unit, got %d", len(got))
	}
}

// ---- wire-level replay ------------------------------------------------------------------

// TestWireLevelReplay drives a canned "clip" (a second of tone chunks, built
// exactly the way wire.py frames them, length prefix included) through the
// door's real message framing and audio path, and validates the recorded
// terminal byte stream against the APC grammar.
func TestWireLevelReplay(t *testing.T) {
	// Producer side: 12 chunks of 80ms S16LE stereo, framed as u32-len + payload.
	var stream bytes.Buffer
	for i := 0; i < 12; i++ {
		pcm := pcmChunk(80, 44100, 2)
		for j := 0; j+1 < len(pcm); j += 2 { // a tone, not just zeros
			binary.LittleEndian.PutUint16(pcm[j:], uint16(int16(3000*((j/40)%2*2-1))))
		}
		p := testMsg(codecRaw, 2, 44100, i, int64(i*80), 80, pcm)
		binary.Write(&stream, binary.BigEndian, uint32(len(p)))
		stream.Write(p)
	}

	// Door side: real framing reader + audio session recording to a fake tty.
	s := enabledSession(defaultAudioConfig())
	var term bytes.Buffer
	s.start(&term)
	now := time.Unix(1000, 0)
	n := 0
	for {
		p, err := readMsg(&stream)
		if err != nil {
			break
		}
		if p[0] != msgAudio {
			t.Fatalf("unexpected message type 0x%02x", p[0])
		}
		m, ok := parseAudioMsg(p)
		if !ok {
			t.Fatalf("chunk %d failed to parse", n)
		}
		if _, err := s.handleChunk(&term, m, now.Add(time.Duration(n*80)*time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 12 {
		t.Fatalf("replayed %d chunks, want 12", n)
	}
	s.stop(&term)

	bodies := assertValidAPCs(t, term.Bytes())
	var stores, loads, queues int
	for _, b := range bodies {
		switch {
		case strings.HasPrefix(b, "SyncTERM:C;S;"):
			stores++
		case strings.HasPrefix(b, "SyncTERM:A;Load;"):
			loads++
		case strings.HasPrefix(b, "SyncTERM:A;Queue;"):
			queues++
		}
	}
	if stores != 12 || loads != 12 || queues != 12 {
		t.Fatalf("store/load/queue = %d/%d/%d, want 12 each", stores, loads, queues)
	}
}
