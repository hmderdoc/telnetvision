// SyncTERM Audio-APC output path: turns AUDIO wire messages into the
// Store/Load/Queue escape-sequence triples a SyncTERM >= 1.10a caller plays
// through libsndfile. Pure byte-shuffling — no native audio dependency; the
// same code runs on every platform the door builds for.
//
// Wire format per the cterm Audio APC doc (verified against SyncTERM's
// term.c/audio_apc.c — note C;S is semicolon-separated, and the audio DSR
// reports only running/stopped, never queue depth):
//
//	ESC _ SyncTERM:C;S;<name>;<base64>       ESC \   (cache the clip)
//	ESC _ SyncTERM:A;Load;S=<slot>;<name>    ESC \   (decode into a slot)
//	ESC _ SyncTERM:A;Queue;C=<ch>;S=<slot>   ESC \   (play; slot empties)
//
// Feature probe: ESC _ SyncTERM:Q;libsndfile ESC \  -> ESC [ = 7 ; 100 ; 0|1 n
// Drain notify:  ESC _ SyncTERM:A;Update;C=<ch> ESC \ arms a one-shot
// ESC [ = 7 ; <ch> ; 0 n emitted when the channel runs dry.
//
// Because the terminal never reports queue depth, backpressure is a local
// ledger (the sequencing model from lameboy's apc_audio.rs): every queued
// chunk adds its duration to emittedMs; lead = emittedMs - wallclock elapsed
// since the anchor; when lead exceeds audio_max_queued chunks' worth we drop
// incoming chunks instead of queueing (drop-to-latest, like the video pacer).
// The drain one-shot re-anchors the ledger after an underrun, and a periodic
// state poll catches a missed one-shot. Per the door's sync policy we never
// stretch or re-declare sample rates — a gap is better than a time-warp.
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

const (
	// Audio channel bounds: 0 and 1 are cterm-owned (ANSI music / SFX) and
	// Queue on them is rejected by the terminal. Never send there.
	audioChanMin = 2
	audioChanMax = 15

	audioFeatureID = 100 // DSR id of the libsndfile feature reply

	featureProbe = "\x1b_SyncTERM:Q;libsndfile\x1b\\"

	// query3DS identifies a 3dBBS caller — the Nintendo 3DS homebrew terminal.
	// It answers `APC 3DS:Ver;<maj>;<min> ST`; every other terminal ignores an
	// unknown APC entirely, so a missing reply simply means "not a handheld".
	//
	// Worth the extra probe because that client cannot take a desktop stream:
	// it drains its 256 KB socket ring through exactly ONE 8192-byte parse per
	// rendered frame (3dBBS source/main.c, `telnetRead(rxbuf, sizeof rxbuf)`),
	// so its intake ceiling is 8 KB x fps — far below what truecolor video
	// plus 22050 Hz audio asks for. Its terminal size is 80x25, the same as a
	// desktop SyncTERM, so nothing else here can tell them apart.
	query3DS = "\x1b_3DS:Query\x1b\\"

	// Grace after the libsndfile reply to let a 3DS:Ver answer land behind it.
	// Both probes are APC jobs on the client's FIFO worker so the replies
	// arrive in order; there is no negative reply to wait on, so this bounded
	// wait is the whole cost of the extra probe on a desktop terminal.
	query3DSGrace = 250 * time.Millisecond

	// Wire AUDIO codec ids (must match wire.py).
	codecRaw  = 0
	codecFLAC = 1
	codecWAV  = 2

	// If no chunk has been queued for this long the audio clock is stale
	// (silent source, producer without audio) and must not gate video.
	audioClockStaleAfter = 2 * time.Second

	audioPollInterval = 250 * time.Millisecond

	// A stopped-report needs a caller round-trip to reach us, so one arriving
	// just after we queued audio describes the PAST — the pre-burst state.
	// Acting on it starts a self-sustaining oscillation: stale report →
	// refill hold → real drain (we stopped feeding!) → another stopped
	// report → … heard as rhythmic ~250ms chop. Reports are only trusted
	// when nothing has been queued for this long.
	drainReportLag = 300 * time.Millisecond

	// Hidden-tail evidence window: the ledger reading lead=0 means the
	// terminal SHOULD be starving — draining between chunks and answering
	// polls with "stopped". Continuous "running" reports across this window
	// while the ledger reads empty are proof of a queue the ledger can't
	// see (the caller's audio clock consuming slower than our wall), i.e. a
	// latency tail the caller hears as video pulling ahead. Trigger a
	// resync on that evidence instead of waiting out the blind interval.
	hiddenTailAfter = 5 * time.Second
)

// audioConfig is the door.ini-tunable surface (all optional; see README).
type audioConfig struct {
	Mode       string // "auto" (feature-detect) | "on" (skip detect) | "off"
	Channel    int    // 2..15
	SlotStart  int    // first slot of the rotating pool
	SlotCount  int    // pool size (slots SlotStart..SlotStart+SlotCount-1)
	MaxQueued  int    // latency budget: max chunks' worth of queued lead
	Prebuffer  int    // cushion: chunks held and queued as one burst (>=1)
	SlackMs    int    // A/V sync tolerance before video is dropped/held
	VolumeDB   int    // A;Volume set once at session start
	ResyncSecs int    // blind tail-cap interval (0 = off; see resyncLocked)
}

func defaultAudioConfig() audioConfig {
	return audioConfig{
		Mode:       "auto",
		Channel:    2,
		SlotStart:  100,
		SlotCount:  100,
		MaxQueued:  6,
		Prebuffer:  3,
		SlackMs:    120,
		VolumeDB:   -6,
		ResyncSecs: 60,
	}
}

// audioConfigFromINI overlays door.ini keys onto the defaults. Bad values
// keep the default rather than erroring — a typo shouldn't kill video.
func audioConfigFromINI(ini map[string]string) audioConfig {
	cfg := defaultAudioConfig()
	if v, ok := ini["audio"]; ok {
		switch v {
		case "auto", "on", "off":
			cfg.Mode = v
		}
	}
	iniInt := func(key string, dst *int) {
		if v, ok := ini[key]; ok {
			if n, err := parseInt(v); err == nil {
				*dst = n
			}
		}
	}
	iniInt("audio_channel", &cfg.Channel)
	iniInt("audio_slot_start", &cfg.SlotStart)
	iniInt("audio_slot_count", &cfg.SlotCount)
	iniInt("audio_max_queued", &cfg.MaxQueued)
	iniInt("audio_prebuffer", &cfg.Prebuffer)
	iniInt("audio_resync_secs", &cfg.ResyncSecs)
	iniInt("sync_slack_ms", &cfg.SlackMs)
	iniInt("audio_volume_db", &cfg.VolumeDB)
	return cfg
}

func parseInt(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(s, "%d", &n)
	return n, err
}

// audioMsg is one parsed wire AUDIO message (type 0x11).
type audioMsg struct {
	Codec      byte
	Channels   byte
	SampleRate uint32
	Seq        uint32
	PtsMs      uint64
	DurMs      uint32
	Payload    []byte
}

// parseAudioMsg decodes a wire AUDIO payload (p[0] == msgAudio). Layout:
// 0x11 | u8 codec | u8 channels | u32 rate | u32 seq | u64 pts | u32 dur | data.
func parseAudioMsg(p []byte) (audioMsg, bool) {
	const hdr = 1 + 1 + 1 + 4 + 4 + 8 + 4
	if len(p) < hdr || p[0] != msgAudio {
		return audioMsg{}, false
	}
	return audioMsg{
		Codec:      p[1],
		Channels:   p[2],
		SampleRate: binary.BigEndian.Uint32(p[3:7]),
		Seq:        binary.BigEndian.Uint32(p[7:11]),
		PtsMs:      binary.BigEndian.Uint64(p[11:19]),
		DurMs:      binary.BigEndian.Uint32(p[19:23]),
		Payload:    p[23:],
	}, true
}

// audioSession owns the door side of one caller's audio stream. Methods are
// safe to call from both the input-reader goroutine (DSR notifications) and
// the render loop (chunk emission) — the mutex covers the ledger.
type audioSession struct {
	cfg     audioConfig
	enabled bool

	// is3DBBS marks a 3dBBS caller (Nintendo 3DS homebrew terminal). It
	// answers Q;libsndfile truthfully, so without a second probe it is
	// indistinguishable from a desktop SyncTERM and gets a desktop-sized
	// stream it cannot physically consume — see the handheld notes on
	// query3DS. Set by detect(); read by the render loop.
	is3DBBS bool

	mu   sync.Mutex
	slot int // next pool index to use (0..SlotCount-1)

	// Playback-timeline ledger (the backpressure + A/V sync source of truth).
	primed     bool
	playStart  time.Time // wall-clock anchor: when the queued timeline began
	emittedMs  int64     // total queued audio duration since the anchor
	lastEndPts int64     // producer-clock pts at the end of the newest chunk
	havePts    bool
	lastQueue  time.Time // when the newest chunk was queued (staleness gate)
	chunkMs    int64     // duration of the most recent chunk (lead budget unit)

	needArm  bool // a drain fired; re-arm A;Update on the next tick
	nextPoll time.Time

	// Cushion: chunks held back until Prebuffer of them exist, then queued as
	// ONE burst so the terminal always has a jitter reserve. Dribbling chunks
	// in one at a time leaves a ~1-chunk cushion — any hiccup longer than
	// that drains the channel and each later clip plays in isolation
	// ("choppy/bitcrushed"). Refilled from scratch after every drain.
	fill     []heldChunk
	fillLast time.Time // when the newest held chunk arrived (stale flush)

	// Pile-up control (see resyncLocked). lastEmpty is the last PROOF the
	// terminal's FIFO was empty (a drain report, a prime, a resync, the
	// initial burst); lastRate/lastChannels remember the stream format so a
	// silence clip can be synthesized at any time. runningEmptySince tracks
	// how long poll replies have said "running" while the ledger read
	// empty — the hidden-tail evidence; needResync carries the verdict from
	// the reader goroutine to the render loop's tick.
	lastEmpty         time.Time
	lastRate          uint32
	lastChannels      byte
	runningEmptySince time.Time
	needResync        bool

	// Caller mute (the M key). Muting must silence the channel NOW — the
	// flush rides the next tick — and must not gate video afterwards.
	muted     bool
	needFlush bool

	// Diagnostics (debug log only).
	primes     int           // silence-primes after stream gaps (each = a gap)
	resyncs    int           // tail flushes (latency pile-up corrections)
	lastRecv   time.Time     // newest chunk arrival
	recvMaxGap time.Duration // worst inter-arrival gap this stats window
	recvN      int           // chunks arrived this stats window
	lastSeq    uint32
	seqValid   bool
	seqSkips   int // cumulative sequence discontinuities (upstream drops)
}

// heldChunk is an encoded, ready-to-emit clip waiting in the cushion.
type heldChunk struct {
	file  []byte
	codec byte
	durMs int64
	pts   int64
}

// newAudioSession builds a session from config, clamping insane values back
// into the ranges the protocol allows (channel 2..15, slots within 0..255).
func newAudioSession(cfg audioConfig) *audioSession {
	if cfg.Channel < audioChanMin || cfg.Channel > audioChanMax {
		cfg.Channel = audioChanMin
	}
	if cfg.SlotStart < 0 || cfg.SlotStart > 255 {
		cfg.SlotStart = 100
	}
	if cfg.SlotCount < 1 || cfg.SlotStart+cfg.SlotCount > 256 {
		cfg.SlotCount = 256 - cfg.SlotStart
	}
	if cfg.MaxQueued < 1 {
		cfg.MaxQueued = 6
	}
	if cfg.Prebuffer < 1 {
		cfg.Prebuffer = 1
	}
	if cfg.Prebuffer > cfg.MaxQueued {
		cfg.Prebuffer = cfg.MaxQueued // a cushion larger than the budget deadlocks
	}
	if cfg.SlackMs < 0 {
		cfg.SlackMs = 120
	}
	return &audioSession{cfg: cfg, chunkMs: 80}
}

// detect runs the one-time feature probe: emit Q;libsndfile and wait up to
// timeout for the ESC[=7;100;<x>n reply. MUST run before the input-reader
// goroutine starts (same rule as the CPR size probe) or the reader swallows
// the reply. mode=off never probes; mode=on trusts the sysop and skips the
// probe. The result is cached in s.enabled — callers never re-probe.
//
// Windows note: readTimeout is a no-op there (same limitation as the size
// probe), so auto can never see the reply and quietly disables audio; a
// sysop who knows the callers use SyncTERM can set `audio = on`.
func (s *audioSession) detect(in, out *os.File, timeout time.Duration) {
	switch s.cfg.Mode {
	case "off":
		s.enabled = false
		return
	case "on":
		s.enabled = true
		return
	}
	// Both probes go out together: a 3dBBS caller answers both, a desktop
	// SyncTERM answers only the first. Pairing them means the handheld check
	// costs nothing extra when it IS a handheld, and at most query3DSGrace
	// when it isn't.
	if _, err := out.Write([]byte(featureProbe + query3DS)); err != nil {
		s.enabled = false
		return
	}
	deadline := time.Now().Add(timeout)
	buf := make([]byte, 0, 128)
	scratch := make([]byte, 64)
	gotFeature, feature := false, false
	var graceEnd time.Time
	for time.Now().Before(deadline) && len(buf) < 512 {
		rem := time.Until(deadline)
		if rem > 50*time.Millisecond {
			rem = 50 * time.Millisecond
		}
		n, err := readTimeout(in, scratch, rem)
		if n > 0 {
			buf = append(buf, scratch[:n]...)
			if !gotFeature {
				if avail, found := parseFeatureReply(buf, audioFeatureID); found {
					gotFeature, feature = true, avail
					graceEnd = time.Now().Add(query3DSGrace)
				}
			}
			if parse3DSVer(buf) {
				// A handheld answered; both replies are in.
				s.enabled = gotFeature && feature
				if !gotFeature {
					// 3dBBS supports libsndfile by contract (PROTOCOL.md s4),
					// so a version reply that overtook the feature reply is
					// still proof the sink works.
					s.enabled = true
				}
				s.is3DBBS = true
				return
			}
		} else if err != nil {
			break
		} else {
			// readTimeout returned empty without sleeping out its window
			// (the Windows no-op); don't busy-spin until the deadline.
			time.Sleep(5 * time.Millisecond)
		}
		if gotFeature && time.Now().After(graceEnd) {
			s.enabled = feature // desktop terminal: no 3DS reply is coming
			return
		}
	}
	if gotFeature {
		s.enabled = feature
		return
	}
	s.enabled = false // no reply: terminal predates Audio APCs
}

// parse3DSVer reports whether buf contains a 3dBBS version reply,
// `ESC _ 3DS:Ver;<maj>;<min> ESC \`. Tolerates surrounding noise: the probe
// reply can arrive mixed with the CPR answer and typed bytes.
func parse3DSVer(buf []byte) bool {
	return bytes.Contains(buf, []byte("\x1b_3DS:Ver;"))
}

// parseDSRPairs decodes the parameter bytes of an audio DSR — the text
// between `ESC[` and the final `n`, e.g. "=7;2;0" — into [id, state] pairs.
// ok is false if the params aren't an audio (=7) report at all.
func parseDSRPairs(params []byte) (pairs [][2]int, ok bool) {
	if len(params) < 2 || params[0] != '=' || params[1] != '7' {
		return nil, false
	}
	j := 2
	var vals []int
	for j < len(params) {
		if params[j] != ';' {
			return nil, false
		}
		j++
		v := -1
		for j < len(params) && params[j] >= '0' && params[j] <= '9' {
			if v < 0 {
				v = 0
			}
			v = v*10 + int(params[j]-'0')
			j++
		}
		if v < 0 {
			return nil, false
		}
		vals = append(vals, v)
	}
	for k := 0; k+1 < len(vals); k += 2 {
		pairs = append(pairs, [2]int{vals[k], vals[k+1]})
	}
	return pairs, true
}

// parseFeatureReply scans a raw byte buffer for ESC [ = 7 ; <id> ; <state> n
// with the given id, tolerating surrounding noise (same contract as
// parseCursorReport). Returns state != 0 and whether a reply was found.
func parseFeatureReply(buf []byte, id int) (avail, found bool) {
	for i := 0; i+4 < len(buf); i++ {
		if buf[i] != 0x1B || buf[i+1] != '[' {
			continue
		}
		j := i + 2
		for j < len(buf) && buf[j] != 'n' && buf[j] != 0x1B && j-i < 64 {
			j++
		}
		if j >= len(buf) || buf[j] != 'n' {
			continue
		}
		if pairs, ok := parseDSRPairs(buf[i+2 : j]); ok {
			for _, p := range pairs {
				if p[0] == id {
					return p[1] != 0, true
				}
			}
		}
	}
	return false, false
}

// start emits the session-start setup (base channel volume) and the first
// drain-notify arm. No-op when audio is disabled.
//
// The dB suffix is load-bearing: audio_apc.c parses a BARE number as linear
// 0..100 percent, so "V=-6" clamps to the -60dB floor (inaudible); decibels
// need the literal "dB" suffix.
func (s *audioSession) start(w io.Writer) error {
	if !s.enabled {
		return nil
	}
	_, err := fmt.Fprintf(w, "\x1b_SyncTERM:A;Volume;C=%d;V=%ddB\x1b\\", s.cfg.Channel, s.cfg.VolumeDB)
	return err
}

// leadMs is the ledger's estimate of audio queued ahead of the terminal's
// playback position. Callers hold s.mu.
func (s *audioSession) leadMsLocked(now time.Time) int64 {
	if !s.primed {
		return 0
	}
	lead := s.emittedMs - now.Sub(s.playStart).Milliseconds()
	if lead < 0 {
		return 0
	}
	return lead
}

// playingPts maps "now" to the producer-clock pts of the audio currently at
// the caller's speaker: the end of the newest queued chunk minus the queued
// lead. ok is false when the clock can't be trusted (nothing queued yet, or
// the stream went quiet) — video must then render ungated.
func (s *audioSession) playingPts(now time.Time) (pts int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || !s.havePts || now.Sub(s.lastQueue) > audioClockStaleAfter {
		return 0, false
	}
	return s.lastEndPts - s.leadMsLocked(now), true
}

// handleChunk applies backpressure and cushioning. While the cushion is
// filling (session start, and again after every drain) chunks are held; once
// Prebuffer of them exist they go out as ONE contiguous burst, so the
// terminal always starts with a jitter reserve. In steady state each chunk
// is emitted immediately as a Store/Load/Queue triple. Returns whether the
// chunk was queued (held-for-cushion counts as not-yet-queued). Raw-PCM
// payloads are wrapped in a WAV header here; FLAC/WAV pass through opaque.
func (s *audioSession) handleChunk(w io.Writer, m audioMsg, now time.Time) (queued bool, err error) {
	if !s.enabled {
		return false, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	// Arrival accounting for the debug log: chunks/sec, worst inter-arrival
	// gap and sequence skips measure the UPSTREAM leg directly (producer →
	// relay → door), independent of anything this session does with them.
	if !s.lastRecv.IsZero() {
		if g := now.Sub(s.lastRecv); g > s.recvMaxGap {
			s.recvMaxGap = g
		}
	}
	s.lastRecv = now
	s.recvN++
	if s.seqValid && m.Seq != s.lastSeq+1 {
		s.seqSkips++
	}
	s.lastSeq, s.seqValid = m.Seq, true
	s.lastRate, s.lastChannels = m.SampleRate, m.Channels

	if s.muted {
		return false, nil // discarded; unmute resumes via the gap prime
	}

	dur := int64(m.DurMs)
	if dur <= 0 {
		dur = s.chunkMs
	}
	// Latency budget: the ledger's lead crossing it means the queue has
	// piled up (a source clock running faster than the wall). Dropping the
	// NEW chunk would keep the stale backlog playing — the latency would
	// never leave. Instead skip to the present: flush the tail, re-prime,
	// and keep this fresh chunk (lameboy's surplus rule).
	if s.leadMsLocked(now) > int64(s.cfg.MaxQueued)*dur {
		if err := s.resyncLocked(w, now); err != nil {
			return false, err
		}
	}

	file := m.Payload
	if m.Codec == codecRaw {
		if s.is3DBBS {
			// Halve the wire cost for the handheld. 3dBBS decodes WAV
			// "PCM16/U8" natively (source/audio/apcaudio.c wavDecode) and
			// answers Q;libsndfileFormat for the U8 subtype with 1, so this
			// is a real format it asked for — not a downgrade it has to
			// resample. 8-bit at 22050 Hz is well matched to its speaker,
			// and unlike lowering the RATE it needs no resampler and cannot
			// drift the clock: sample count and duration are unchanged.
			file = encodeWAV8(m.Payload, int(m.SampleRate), int(m.Channels))
		} else {
			file = encodeWAV(m.Payload, int(m.SampleRate), int(m.Channels))
		}
	}
	if s.fillingLocked() {
		s.fill = append(s.fill, heldChunk{file: file, codec: m.Codec, durMs: dur, pts: int64(m.PtsMs)})
		s.fillLast = now
		if len(s.fill) < s.cfg.Prebuffer {
			return false, nil // held; the burst goes out when the cushion is full
		}
		return true, s.flushFillLocked(w, now)
	}
	// Silence-prime: the stream went quiet long enough that the terminal ran
	// (or will run) dry. Holding real chunks to rebuild a cushion would ADD
	// a gap and can deadlock against the report guard — instead, lameboy
	// style, lead with a short silent clip: the reserve rebuilds while the
	// real audio flows through untouched.
	if !s.lastQueue.IsZero() && now.Sub(s.lastQueue) > drainReportLag {
		s.playStart = now
		s.emittedMs = 0
		if err := s.primeSilenceLocked(w, now); err != nil {
			return false, err
		}
		// The drain one-shot fired during the gap; re-arm with the resume.
		if err := s.armUpdateLocked(w); err != nil {
			return false, err
		}
		s.needArm = false
		s.primes++
	}
	if err := s.emitChunkLocked(w, file, m.Codec); err != nil {
		return false, err
	}
	s.emittedMs += dur
	s.lastEndPts = int64(m.PtsMs) + dur
	s.havePts = true
	s.lastQueue = now
	s.chunkMs = dur
	return true, nil
}

// primeSilenceLocked queues one silent clip the size of the configured
// cushion, so playback resumes (or continues after a resync) with a jitter
// reserve without ever withholding real audio. Counts as proof the FIFO was
// (near) empty. No-op before the first chunk reveals the stream format.
func (s *audioSession) primeSilenceLocked(w io.Writer, now time.Time) error {
	if s.lastRate == 0 {
		return nil
	}
	silMs := int64(s.cfg.Prebuffer) * s.chunkMs
	if silMs > 400 {
		silMs = 400
	}
	n := int(s.lastRate) * int(s.lastChannels) * 2 * int(silMs) / 1000
	// Same format as the real chunks: the handheld's 8-bit path costs half
	// the bytes, and a prime is emitted on every gap so it is not rare.
	// (Silence is not all-zeroes in 8-bit WAV — encodeWAV8 biases it to 128.)
	var sil []byte
	if s.is3DBBS {
		sil = encodeWAV8(make([]byte, n), int(s.lastRate), int(s.lastChannels))
	} else {
		sil = encodeWAV(make([]byte, n), int(s.lastRate), int(s.lastChannels))
	}
	if err := s.emitChunkLocked(w, sil, codecWAV); err != nil {
		return err
	}
	s.emittedMs += silMs
	s.lastQueue = now
	s.lastEmpty = now
	return nil
}

// resyncLocked is lameboy's periodic "engine restart", ported: when the
// terminal's channel FIFO has accumulated an unplayed tail (a source clock
// slightly faster than the playback clock adds a growing latency the drain
// one-shot can never report, because the queue never empties), hard-reset
// it — Flush the whole head..tail backlog, re-anchor the ledger to
// "queued == 0 now", re-arm the drain notify (the flush idles the channel
// and can itself trip a report), and re-prime the silence cushion so
// playback resumes without a click. The cost is one brief skip; the win is
// audio latency capped at the budget instead of growing without bound —
// which the caller sees as video pulling further and further ahead.
func (s *audioSession) resyncLocked(w io.Writer, now time.Time) error {
	if !s.primed {
		return nil
	}
	if _, err := fmt.Fprintf(w, "\x1b_SyncTERM:A;Flush;C=%d\x1b\\", s.cfg.Channel); err != nil {
		return err
	}
	s.playStart = now
	s.emittedMs = 0
	s.needArm = false
	if err := s.armUpdateLocked(w); err != nil {
		return err
	}
	if err := s.primeSilenceLocked(w, now); err != nil {
		return err
	}
	s.resyncs++
	s.runningEmptySince = time.Time{}
	s.needResync = false
	return nil
}

// fillingLocked reports whether the cushion is being (re)built: before the
// first burst, and again after every drain report.
func (s *audioSession) fillingLocked() bool { return !s.primed || s.fill != nil }

// flushFillLocked queues the whole held cushion as one contiguous burst,
// re-anchors the ledger at the burst, and (re)arms the drain one-shot.
func (s *audioSession) flushFillLocked(w io.Writer, now time.Time) error {
	s.primed = true
	s.playStart = now
	s.emittedMs = 0
	s.needArm = false
	if err := s.armUpdateLocked(w); err != nil {
		return err
	}
	for _, c := range s.fill {
		if err := s.emitChunkLocked(w, c.file, c.codec); err != nil {
			return err
		}
		s.emittedMs += c.durMs
		s.lastEndPts = c.pts + c.durMs
		s.chunkMs = c.durMs
	}
	s.havePts = true
	s.lastQueue = now
	s.lastEmpty = now // the burst began on an empty channel
	s.fill = nil
	return nil
}

// emitChunkLocked writes one full Store/Load/Queue triple for an
// already-encoded audio file (WAV or FLAC bytes) into w as ONE contiguous
// byte sequence, and rotates the slot pool. The filename reuses the slot
// index (tv_<slot>.<ext>) so the caller's per-session cache directory holds
// at most SlotCount files instead of growing per chunk.
func (s *audioSession) emitChunkLocked(w io.Writer, file []byte, codec byte) error {
	slot := s.cfg.SlotStart + s.slot
	name := fmt.Sprintf("tv_%d.%s", slot, extForCodec(codec))
	s.slot = (s.slot + 1) % s.cfg.SlotCount

	b64 := base64.StdEncoding.EncodeToString(file)
	// One buffer, one Write: the C;S / Load / Queue triple must arrive
	// contiguously so a slot is never queued before its data landed.
	seq := make([]byte, 0, len(b64)+96)
	seq = append(seq, "\x1b_SyncTERM:C;S;"+name+";"...)
	seq = append(seq, b64...)
	seq = append(seq, "\x1b\\"...)
	seq = append(seq, fmt.Sprintf("\x1b_SyncTERM:A;Load;S=%d;%s\x1b\\", slot, name)...)
	seq = append(seq, fmt.Sprintf("\x1b_SyncTERM:A;Queue;C=%d;S=%d\x1b\\", s.cfg.Channel, slot)...)
	_, err := w.Write(seq)
	return err
}

func (s *audioSession) armUpdateLocked(w io.Writer) error {
	_, err := fmt.Fprintf(w, "\x1b_SyncTERM:A;Update;C=%d\x1b\\", s.cfg.Channel)
	return err
}

// toggleMute flips the caller's mute (the M key) and reports the new state.
// Called from the input-reader goroutine; the channel flush that actually
// silences playback rides the render loop's next tick. Unmute needs no
// action here — the next chunk resumes through the normal gap-prime path.
func (s *audioSession) toggleMute() (muted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return false
	}
	s.muted = !s.muted
	if s.muted {
		s.needFlush = s.primed
		s.havePts = false // release the video gate; picture free-runs
		s.runningEmptySince = time.Time{}
		s.needResync = false
	}
	return s.muted
}

// onDSR digests audio DSR pairs from the input reader. A state-0 report for
// our channel means the FIFO drained (the armed one-shot, or a poll reply
// finding the channel idle): the ledger's lead is stale, so re-anchor to
// "queued == 0 now" and schedule a re-arm. Called from the reader goroutine;
// the actual re-arm bytes go out on the render loop's next tick.
func (s *audioSession) onDSR(pairs [][2]int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range pairs {
		if p[0] != s.cfg.Channel || !s.primed {
			continue
		}
		if p[1] != 0 {
			// Running. If the ledger reads empty, the terminal is playing
			// audio we no longer account for — a hidden tail. Transient
			// mismatches (poll round-trip staleness) don't survive the
			// evidence window; a persistent one convicts.
			if s.leadMsLocked(now) == 0 {
				if s.runningEmptySince.IsZero() {
					s.runningEmptySince = now
				} else if now.Sub(s.runningEmptySince) > hiddenTailAfter {
					s.needResync = true
				}
			} else {
				s.runningEmptySince = time.Time{}
			}
			continue
		}
		s.runningEmptySince = time.Time{} // stopped: no hidden tail
		// Staleness guard: audio queued more recently than a report's
		// possible round-trip cannot have drained — the report predates the
		// queue. A GENUINE underrun always means chunks stopped flowing, so
		// lastQueue ages past the guard and the next report is trusted.
		if now.Sub(s.lastQueue) < drainReportLag {
			continue
		}
		s.playStart = now
		s.emittedMs = 0
		s.needArm = true
		s.lastEmpty = now // a trusted drain report IS proof of empty
	}
}

// tick appends periodic maintenance output to w: a pending Update re-arm and
// the 250ms channel-state poll (CSI = 7 ; <ch> n) that catches a missed
// drain one-shot. Call from the render loop when the output pacer is idle.
func (s *audioSession) tick(w io.Writer, now time.Time) error {
	if !s.enabled {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Mute pressed: silence the channel immediately (don't wait for the
	// queued tail to play out), then idle until unmute.
	if s.needFlush {
		s.needFlush = false
		if _, err := fmt.Fprintf(w, "\x1b_SyncTERM:A;Flush;C=%d\x1b\\", s.cfg.Channel); err != nil {
			return err
		}
		s.playStart = now
		s.emittedMs = 0
		s.lastEmpty = now
	}
	if s.muted {
		return nil // no polls, no resyncs, no primes while muted
	}
	// A partially-filled cushion that stopped growing (sparse or ended
	// source) goes out as-is — held audio must never wait forever.
	if len(s.fill) > 0 && now.Sub(s.fillLast) > 300*time.Millisecond {
		if err := s.flushFillLocked(w, now); err != nil {
			return err
		}
	}
	// Evidence-driven tail cap: the poll replies convicted a hidden tail
	// (running-while-ledger-empty persisted past the window). Resync now —
	// this catches caller-clock pile-up within seconds.
	if s.needResync {
		s.needResync = false
		if err := s.resyncLocked(w, now); err != nil {
			return err
		}
	}
	// Blind tail cap: the fallback for terminals that don't answer polls.
	// A full interval with NO proof the channel was ever empty (no drain,
	// no prime, no resync — continuous playback) means a tail may be
	// accumulating; reset it on suspicion, like lameboy's periodic resync.
	// Intervals with normal drains never trigger this.
	if s.primed && s.cfg.ResyncSecs > 0 && !s.lastEmpty.IsZero() &&
		now.Sub(s.lastEmpty) > time.Duration(s.cfg.ResyncSecs)*time.Second {
		if err := s.resyncLocked(w, now); err != nil {
			return err
		}
	}
	if s.needArm {
		s.needArm = false
		if err := s.armUpdateLocked(w); err != nil {
			return err
		}
	}
	if s.primed && now.After(s.nextPoll) {
		s.nextPoll = now.Add(audioPollInterval)
		if _, err := fmt.Fprintf(w, "\x1b[=7;%dn", s.cfg.Channel); err != nil {
			return err
		}
	}
	return nil
}

// stats returns a one-line health snapshot for the debug log: queued lead vs
// the terminal, arrival health (rate/gaps/sequence skips), and the pile-up
// counters (primes = gaps recovered, resyncs = latency tails flushed).
func (s *audioSession) stats(now time.Time) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled {
		return "audio off"
	}
	state := "steady"
	if s.muted {
		state = "muted"
	} else if s.fillingLocked() {
		state = fmt.Sprintf("filling %d/%d", len(s.fill), s.cfg.Prebuffer)
	}
	line := fmt.Sprintf("audio: lead=%dms recv=%d maxgap=%dms skips=%d primes=%d resyncs=%d state=%s",
		s.leadMsLocked(now), s.recvN, s.recvMaxGap.Milliseconds(),
		s.seqSkips, s.primes, s.resyncs, state)
	s.recvN = 0
	s.recvMaxGap = 0
	return line
}

// videoGate verdicts: render the frame, drop it (stale), or hold it (early).
const (
	gateRender = iota
	gateDrop
	gateHold
)

// videoGate compares a video frame's producer pts to the audio clock. When
// the audio clock is unavailable (audio off, nothing queued, stream stale)
// video renders ungated — silence must never freeze the picture.
func (s *audioSession) videoGate(ptsMs int64, now time.Time) int {
	apts, ok := s.playingPts(now)
	if !ok {
		return gateRender
	}
	d := ptsMs - apts
	slack := int64(s.cfg.SlackMs)
	if d < -slack {
		return gateDrop
	}
	if d > slack {
		return gateHold
	}
	return gateRender
}

// stop emits the session-end flush so no queued tail plays after the door
// returns to the BBS menu. Plain immediate Flush, no fade: the fade variant
// works by queueing ANOTHER buffer to decay the head (alpha-terminal
// machinery), and a carry-over risk at exit isn't worth a 50ms nicety.
func (s *audioSession) stop(w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.enabled || !s.primed {
		return nil
	}
	_, err := fmt.Fprintf(w, "\x1b_SyncTERM:A;Flush;C=%d\x1b\\", s.cfg.Channel)
	return err
}

// extForCodec maps a wire codec id to the cache filename extension the
// terminal's libsndfile sniffs. Raw PCM is wrapped into a WAV container by
// the door (the payload is opaque to the service, not to us).
func extForCodec(codec byte) string {
	switch codec {
	case codecFLAC:
		return "flac"
	default:
		return "wav"
	}
}

// encodeWAV wraps raw interleaved S16LE PCM in a minimal canonical RIFF/WAVE
// header so the terminal's libsndfile can decode it.
func encodeWAV(pcm []byte, rate, channels int) []byte {
	return encodeWAVBits(pcm, rate, channels, 16)
}

// encodeWAV8 converts interleaved S16LE PCM to unsigned 8-bit and wraps it as
// an 8-bit RIFF/WAVE — half the bytes on the wire for the same duration and
// sample rate. WAV's 8-bit sample format is UNSIGNED (centre 128), unlike
// 16-bit's signed, so each sample is shifted and biased rather than truncated.
func encodeWAV8(pcm []byte, rate, channels int) []byte {
	out := make([]byte, len(pcm)/2)
	for i := range out {
		// The high byte of a little-endian S16 is the sample scaled to 8 bits,
		// still signed; flipping its sign bit is exactly the signed->unsigned
		// bias. Interleaving survives because every frame shrinks 2:1 alike.
		out[i] = pcm[i*2+1] ^ 0x80
	}
	return encodeWAVBits(out, rate, channels, 8)
}

func encodeWAVBits(pcm []byte, rate, channels, bits int) []byte {
	if channels < 1 {
		channels = 1
	}
	byteRate := rate * channels * bits / 8
	blockAlign := channels * bits / 8
	v := make([]byte, 0, 44+len(pcm))
	u32 := func(n int) []byte {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(n))
		return b[:]
	}
	u16 := func(n int) []byte {
		var b [2]byte
		binary.LittleEndian.PutUint16(b[:], uint16(n))
		return b[:]
	}
	v = append(v, "RIFF"...)
	v = append(v, u32(36+len(pcm))...)
	v = append(v, "WAVE"...)
	v = append(v, "fmt "...)
	v = append(v, u32(16)...)
	v = append(v, u16(1)...) // PCM
	v = append(v, u16(channels)...)
	v = append(v, u32(rate)...)
	v = append(v, u32(byteRate)...)
	v = append(v, u16(blockAlign)...)
	v = append(v, u16(bits)...)
	v = append(v, "data"...)
	v = append(v, u32(len(pcm))...)
	v = append(v, pcm...)
	return v
}
