#!/usr/bin/env python3
"""
BBSproxy.py - CP437 + ANSI-music + SyncTERM-APC-audio shim for iTerm2.

Output path:
  - CP437 low/high glyph mapping -> Unicode (controls left intact);
  - ANSI music (CSI M/N/|) -> square-wave -> afplay (legacy, monophonic);
  - SyncTERM audio APCs (ESC_ SyncTERM:... ST) -> a real streaming PCM mixer
    (sounddevice), for Game-Boy-quality PCM streamed as chunked clips.

Modes:
    BBSproxy.py <host> [port]            telnet (built-in)
    BBSproxy.py ssh <user@host> [args]   ssh in a pty
    BBSproxy.py --exec <cmd ...>         wrap any command in a pty
    BBSproxy.py --filter                 output-only filter for a pipe
    BBSproxy.py --test-music 'T120O4L8CDE'
    BBSproxy.py --test-apc [file.wav] [--chunk-ms N] [--prebuffer K]

APC audio needs:  pip install sounddevice numpy
"""

import os
import sys
import pty
import math
import base64
import socket
import select
import struct
import wave
import io
import tempfile
import subprocess
import threading
import collections
import time

# Optional heavy deps: only needed for the APC streaming sink. Telnet/ssh/
# filter/ANSI-music all work without them.
try:
    import numpy as np
    import sounddevice as sd
    HAVE_AUDIO = True
except Exception:
    HAVE_AUDIO = False

DEVICE_RATE = 44100
DEVICE_CH = 2


def _default_output_signature():
    """Stable-enough identity for the current PortAudio default output."""
    try:
        info = sd.query_devices(kind="output")
        return (info.get("name"), info.get("hostapi"),
                info.get("max_output_channels"), info.get("default_samplerate"))
    except Exception:
        return None

# ----------------------------------------------------------------------------
# CP437 low-glyph table (0x01-0x1F). NUL (0x00) is dropped.
# ----------------------------------------------------------------------------
CP437_LOW = {
    0x01: "\u263A", 0x02: "\u263B", 0x03: "\u2665", 0x04: "\u2666",
    0x05: "\u2663", 0x06: "\u2660", 0x07: "\u2022", 0x08: "\u25D8",
    0x09: "\u25CB", 0x0A: "\u25D9", 0x0B: "\u2642", 0x0C: "\u2640",
    0x0D: "\u266A", 0x0E: "\u266B", 0x0F: "\u263C", 0x10: "\u25BA",
    0x11: "\u25C4", 0x12: "\u2195", 0x13: "\u203C", 0x14: "\u00B6",
    0x15: "\u00A7", 0x16: "\u25AC", 0x17: "\u21A8", 0x18: "\u2191",
    0x19: "\u2193", 0x1A: "\u2192", 0x1B: "\u2190", 0x1C: "\u221F",
    0x1D: "\u2194", 0x1E: "\u25B2", 0x1F: "\u25BC",
}
LIVE_CONTROLS = {0x07, 0x08, 0x09, 0x0A, 0x0C, 0x0D, 0x1B}


# ----------------------------------------------------------------------------
# Minimal telnet IAC handling (built-in telnet client mode only).
# ----------------------------------------------------------------------------
IAC, DONT, DO, WONT, WILL, SB, SE = 255, 254, 253, 252, 251, 250, 240
OPT_BINARY, OPT_ECHO, OPT_SGA, OPT_TTYPE = 0, 1, 3, 24
WE_SUPPORT = {OPT_BINARY, OPT_SGA, OPT_ECHO, OPT_TTYPE}
TTYPE_IS, TTYPE_SEND = 0, 1
# Terminal type reported to the BBS via telnet TTYPE. The proxy used to answer
# nothing, so the BBS fell back to the account default ("ansi"); report a
# full-featured type so it doesn't degrade capabilities (e.g. kitty keyboard).
TERM_TYPE = b"xterm-256color"


class Telnet:
    def __init__(self):
        self.state = "data"
        self.cmd = None
        self.agreed = set()
        self.sb = bytearray()

    def feed(self, data):
        out, reply = bytearray(), bytearray()
        for b in data:
            if self.state == "data":
                if b == IAC:
                    self.state = "iac"
                else:
                    out.append(b)
            elif self.state == "iac":
                if b == IAC:
                    out.append(IAC); self.state = "data"
                elif b in (DO, DONT, WILL, WONT):
                    self.cmd = b; self.state = "opt"
                elif b == SB:
                    self.sb = bytearray(); self.state = "sb"
                else:
                    self.state = "data"
            elif self.state == "opt":
                self._negotiate(self.cmd, b, reply); self.state = "data"
            elif self.state == "sb":
                if b == IAC:
                    self.state = "sb_iac"
                else:
                    self.sb.append(b)
            elif self.state == "sb_iac":
                if b == SE:
                    self._subneg(bytes(self.sb), reply); self.state = "data"
                elif b == IAC:
                    self.sb.append(IAC); self.state = "sb"   # escaped 0xFF in payload
                else:
                    self.state = "sb"
        return bytes(out), bytes(reply)

    def _negotiate(self, cmd, opt, reply):
        if cmd == DO:
            if opt in WE_SUPPORT and opt not in self.agreed:
                self.agreed.add(opt); reply += bytes((IAC, WILL, opt))
            elif opt not in WE_SUPPORT:
                reply += bytes((IAC, WONT, opt))
        elif cmd == WILL:
            if opt in WE_SUPPORT and opt not in self.agreed:
                self.agreed.add(opt); reply += bytes((IAC, DO, opt))
            elif opt not in WE_SUPPORT:
                reply += bytes((IAC, DONT, opt))

    def _subneg(self, sb, reply):
        # Answer "send your terminal type": IAC SB TTYPE IS <TERM_TYPE> IAC SE.
        # We report the same type each time; the server stops when it repeats.
        if len(sb) >= 2 and sb[0] == OPT_TTYPE and sb[1] == TTYPE_SEND:
            reply += bytes((IAC, SB, OPT_TTYPE, TTYPE_IS)) + TERM_TYPE \
                + bytes((IAC, SE))


# ----------------------------------------------------------------------------
# Keyboard bridge. Two modes:
#
# DEFAULT (native kitty): a transparent pipe for keyboard control traffic. The
# door's capability probes (CTerm CTDA, kitty query, DA1) and its kitty
# push/pop reach iTerm unmodified, and iTerm's replies/key events flow back
# untouched -- so the door negotiates the kitty keyboard protocol itself and
# decodes press/release edges natively (arrows included). This is the
# SyncTERM-quality path for iTerm2 and any kitty-capable terminal.
#
# --cterm-bridge (fallback): present iTerm to the BBS as a SyncTERM-like
# terminal. Answer the door's CTDA with "8" (physical key reports), swallow its
# kitty query, and when it turns on CTerm reports (CSI = 1 h) enable kitty on
# iTerm and re-emit key events as CTerm evdev edges (CSI = <code> K/k) --
# buttons AND arrows. Kept for doors that only speak the CTerm fork, and as
# the reference for the fTelnet patch.
# ----------------------------------------------------------------------------
# kitty Unicode keysym -> Linux evdev code, for the keys the door maps.
KITTY_CP_TO_EVDEV = {
    122: 44,   # z -> Z (A)
    120: 45,   # x -> X (B)
    13: 28,    # Enter -> Start
    32: 57,    # Space -> Select
    27: 1,     # Esc (quit)
    113: 16,   # q (quit)
    112: 25,   # p (cycle DMG palette)
    97: 30,    # a (accept multiplayer challenge)
}
# kitty arrow finals -> evdev codes (same codes SyncTERM reports).
ARROW_TO_EVDEV = {0x41: 103, 0x42: 108, 0x43: 106, 0x44: 105}  # A/B/C/D
def _apc_volume(value, default=1.0):
    """Canonical SyncTERM volume: bare number = 0..100 linear percent,
    'NdB' suffix = decibels. Returns a linear gain multiplier."""
    try:
        s = str(value).strip()
        if s.lower().endswith("db"):
            return 10.0 ** (float(s[:-2]) / 20.0)
        v = float(s)
    except (TypeError, ValueError):
        return default
    if v <= 0:
        return 0.0
    if v > 100.0:
        v = 100.0
    return v / 100.0


def _kitty_first_int(body):
    """First field of a kitty CSI body (the key codepoint), e.g. '122;1:3'->122."""
    f = body.decode("latin1").split(";")[0].split(":")[0]
    try:
        return int(f)
    except ValueError:
        return -1


def _kitty_event_type(body):
    """kitty event type: the colon suffix of the 2nd field. 1=press 2=repeat
    3=release; default 1 when absent."""
    parts = body.decode("latin1").split(";")
    if len(parts) >= 2 and ":" in parts[1]:
        try:
            return int(parts[1].split(":")[1])
        except ValueError:
            return 1
    return 1


class KeyboardBridge:
    KITTY_ON = b"\x1b[>10u"   # flags 2|8: report event types + all keys as escapes
    KITTY_OFF = b"\x1b[<u"    # pop the kitty flags (restore iTerm)

    def __init__(self, cterm_bridge=False):
        self.cterm_bridge = cterm_bridge  # False: native-kitty transparent pipe
        self.physical = False    # door requested CSI=1h -> translate kitty to CTerm
        self.kitty_confirmed = False  # iTerm answered our kitty query (mode is live)
        self.down = set()        # evdev codes currently held (edge de-dup)
        self._st = "data"        # terminal->door parser state
        self._csi = bytearray()

    # ---- door -> terminal: intercept CTerm control sequences ----------------
    def handle_door_csi(self, csi):
        """Given a full CSI sequence from the door (b'\\x1b[...<final>'), return
        (forward_to_terminal, reply_to_door)."""
        if not self.cterm_bridge:
            return csi, b""                    # native kitty: transparent pipe
        body = csi[2:-1]
        final = csi[-1:]
        if final == b"c" and body[:1] == b"<":
            return b"", b"\x1b[<0;8c"          # CTDA -> advertise physical keys (8)
        if final == b"u" and body[:1] == b"?":
            return b"", b""                    # swallow kitty query (door uses CTerm)
        if csi == b"\x1b[=1h":
            self.physical = True
            # Enable kitty on iTerm AND query it back: bare arrow presses are
            # only convertible to press edges once we know the mode actually
            # took (a profile with kitty disabled must degrade to passthrough,
            # never to press-only stuck keys).
            return self.KITTY_ON + b"\x1b[?u", b""
        if csi == b"\x1b[=1l":
            self.physical = False
            self.kitty_confirmed = False
            self.down.clear()
            return self.KITTY_OFF, b""         # disable kitty on iTerm
        if csi in (b"\x1b[=2h", b"\x1b[=2l"):
            return b"", b""                    # translated-input toggle: just swallow
        return csi, b""                        # not ours -> forward unchanged

    # ---- terminal -> door: translate kitty key events, pass everything else --
    def from_terminal(self, data):
        if not (self.cterm_bridge and self.physical):
            return data                        # native kitty / legacy: untouched
        out = bytearray()
        for b in data:
            self._in_byte(b, out)
        return bytes(out)

    def _in_byte(self, b, out):
        # Buffer escape sequences whole; pass through anything we don't translate
        # (raw keys, bare legacy arrows, cursor/DA replies, ...) so a terminal that
        # ISN'T in kitty mode -- and the door's screen-size CPR -- keep working.
        if self._st == "data":
            if b == 0x1b:
                self._st = "esc"; self._csi = bytearray((0x1b,))
            else:
                out.append(b)                  # raw byte -> pass through
        elif self._st == "esc":
            self._csi.append(b)
            if b == 0x5b:                      # ESC [
                self._st = "csi"
            else:                              # ESC <other> -> pass through whole
                out += self._csi; self._st = "data"
        elif self._st == "csi":
            self._csi.append(b)
            if 0x40 <= b <= 0x7e:              # final byte
                self._finish_csi(b, out)
                self._st = "data"

    def _finish_csi(self, final, out):
        seq = bytes(self._csi)                 # full b'\x1b[...<final>'
        body = seq[2:-1]
        # Kitty CSI-u key event (numeric codepoint, 'u') -> evdev edge.
        if final == 0x75 and body[:1].isdigit():
            code = KITTY_CP_TO_EVDEV.get(_kitty_first_int(body))
            if code is not None:
                out += self._edge_bytes(code, _kitty_event_type(body))
                return
        # Our kitty query reply (CSI ? flags u): the mode is live. Swallow it --
        # the door never asked (its own query was swallowed in handle_door_csi).
        if final == 0x75 and body[:1] == b"?":
            self.kitty_confirmed = True
            return
        # Arrows -> evdev edges, so a held direction survives chords (macOS
        # auto-repeat stops when another key is pressed; only real press/release
        # edges preserve run momentum through a jump). Repeats are dropped.
        # A *bare* arrow (no kitty event-type) is only a provable key-down when
        # the kitty mode is confirmed; before that it passes through untouched.
        if final in ARROW_TO_EVDEV:
            has_event = b":" in body
            if has_event or self.kitty_confirmed:
                event = _kitty_event_type(body)
                if event != 2:                 # drop repeats: the edge holds
                    out += self._edge_bytes(ARROW_TO_EVDEV[final], event)
                return
        # Everything else -- CPR (...R), DA (...c), legacy arrows pre-confirm --
        # passes through untouched.
        out += seq

    def _edge_bytes(self, code, event):
        if event == 3:                         # release -> up edge
            if code in self.down:
                self.down.discard(code)
                return b"\x1b[=%dk" % code
            return b""
        if code not in self.down:              # press -> down edge (once)
            self.down.add(code)
            return b"\x1b[=%dK" % code
        return b""                             # repeat: nothing


# ----------------------------------------------------------------------------
# Streaming PCM mixer (the new APC sink). A sounddevice callback pulls from each
# channel's gapless FIFO of decoded clips. Queue() appends clips; the callback
# plays them back-to-back with no seam, which is the whole point of the exercise.
# ----------------------------------------------------------------------------
# The FIFO is intentionally NOT capped here. The door (apc_audio.rs) is a closed-
# loop controller: it emits only realtime+cushion worth of audio and compresses
# or drops its OWN backlog, so this queue naturally sits at ~one cushion and never
# runs away -- PROVIDED we send back the one feedback signal it depends on: the
# drain notification CSI=7;<ch>;0n, armed by `A;Update`, fired when a channel's
# FIFO empties. SyncTERM's audio_apc.c sends it; the shim never did, so the door
# ran open-loop into iTerm and could not recover from an underrun (audio that
# "dies and never comes back"). Dropping clips here only starved it faster,
# because the door cannot see clips we silently discard. See poll_drain_notify().


class _Channel:
    def __init__(self):
        self.queue = collections.deque()   # decoded clips: float32 (n, 2)
        self.pos = 0
        self.volume = 1.0

    def frames_pending(self):
        if not self.queue:
            return 0
        total = -self.pos
        for a in self.queue:
            total += a.shape[0]
        return total

    def mix_into(self, out, frames):
        filled = 0
        while filled < frames and self.queue:
            cur = self.queue[0]
            avail = cur.shape[0] - self.pos
            take = avail if avail < (frames - filled) else (frames - filled)
            out[filled:filled + take] += cur[self.pos:self.pos + take] * self.volume
            self.pos += take
            filled += take
            if self.pos >= cur.shape[0]:
                self.queue.popleft()
                self.pos = 0
        # Underrun (queue ran dry mid-block) just leaves silence -> audible gap.
        # That's the signal you're not feeding the FIFO fast enough.


# --- drain-notification feedback (mirrors SyncTERM audio_apc.c) --------------
# The door arms a one-shot per channel with `A;Update;C=<ch>`; when that channel
# plays out and its FIFO goes idle we fire exactly one `CSI=7;<ch>;0n` back and
# disarm. The door parses it (keys.rs parse_audio_drain) and re-anchors its
# timeline + re-primes the cushion (apc_audio.rs notify_drain), which is how it
# recovers from an underrun. Edge-triggered on the play->idle transition, one shot.
def arm_drain(ch, channels, arm, last):
    """Handle A;Update;C=<ch>: arm the drain notify. Seed last-state to the
    current state so we don't fire a spurious edge before any audio is queued."""
    arm[ch] = True
    last[ch] = 1 if (ch in channels and channels[ch].frames_pending() > 0) else 0


def poll_drain_notify(channels, arm, last):
    """Return the notify bytes for every armed channel that just went idle, then
    disarm it. Empty bytes when nothing drained. Call once per audio callback."""
    out = bytearray()
    for ch in list(arm.keys()):
        if not arm[ch]:
            continue
        state = 1 if (ch in channels and channels[ch].frames_pending() > 0) else 0
        if last.get(ch, 0) == 1 and state == 0:   # play -> idle edge
            out += b"\x1b[=7;%d;0n" % ch
            arm[ch] = False
        last[ch] = state
    return bytes(out)


class AudioEngine:
    def __init__(self):
        self.lock = threading.Lock()
        self.cache = {}       # filename -> raw file bytes (from Store)
        self.slots = {}       # slot id -> decoded float32 (n, 2) @ DEVICE_RATE
        self.channels = {}    # channel id -> _Channel
        self.underruns = 0
        self.notify_arm = {}  # channel -> Update-armed one-shot flag
        self.notify_last = {} # channel -> last-polled running state (edge detect)
        self.notify_out = bytearray()  # pending CSI=7;ch;0n bytes for the door
        self.stream = None
        self.callback_status = ""
        self.device_signature = None
        self.last_device_check = 0.0
        self.device_changed = False
        self._open_stream()

    def _open_stream(self):
        """Open the current default output device.

        PortAudio binds a stream to the default device as it exists at stream
        creation time. Recreating it is therefore required after headphones,
        displays, or aggregate devices are switched.
        """
        stream = sd.OutputStream(
            samplerate=DEVICE_RATE, channels=DEVICE_CH, dtype="float32",
            latency="low", callback=self._cb)
        try:
            stream.start()
        except Exception:
            try:
                stream.close()
            except Exception:
                pass
            raise
        self.stream = stream
        self.callback_status = ""
        self.device_signature = _default_output_signature()
        self.last_device_check = time.monotonic()
        self.device_changed = False

    def is_healthy(self):
        stream = self.stream
        if stream is None:
            return False
        try:
            if not stream.active:
                return False
        except Exception:
            return False
        now = time.monotonic()
        if now - self.last_device_check >= 1.0:
            self.last_device_check = now
            current = _default_output_signature()
            if (current is not None and self.device_signature is not None and
                    current != self.device_signature):
                self.device_changed = True
        return not self.device_changed

    def reopen(self):
        """Replace only the device stream, preserving queued mixer audio."""
        old, self.stream = self.stream, None
        if old is not None:
            try:
                old.stop()
            except Exception:
                pass
            try:
                old.close()
            except Exception:
                pass
        self._open_stream()

    def _cb(self, outdata, frames, time_info, status):
        if status:
            # Do not write stderr from PortAudio's realtime callback. The main
            # thread reports this if the stream subsequently needs reopening.
            self.callback_status = str(status)
        outdata.fill(0.0)
        with self.lock:
            for ch in self.channels.values():
                ch.mix_into(outdata, frames)
            # After mixing, report any channel that just drained so the door can
            # re-anchor. Bytes are buffered here and flushed to the door by the
            # main loop -- never written to the socket from this audio thread.
            self.notify_out += poll_drain_notify(
                self.channels, self.notify_arm, self.notify_last)
        np.clip(outdata, -1.0, 1.0, out=outdata)

    def take_notify(self):
        """Pull and clear pending drain-notify bytes (main loop -> door)."""
        with self.lock:
            if not self.notify_out:
                return b""
            data = bytes(self.notify_out)
            self.notify_out = bytearray()
            return data

    # --- APC dispatch ------------------------------------------------------
    def handle_apc(self, rest):
        """rest is the bytes after 'SyncTERM:' e.g. b'A;Queue;C=2;S=0' or
        b'C;S;c0;<base64>'. Separators verified against SyncTERM's term.c:
        C;S is semicolon-separated (strncmp "SyncTERM:C;S;")."""
        parts = rest.split(b";")
        cat = parts[0:1] and parts[0]
        if cat == b"C" and len(parts) >= 4 and parts[1] == b"S":
            name = parts[2].decode("latin1")
            data = b";".join(parts[3:])        # base64 has no ';'
            self._store(name, base64.b64decode(data))
            return
        if cat != b"A" or len(parts) < 2:
            return
        cmd = parts[1].decode("latin1")
        kv, pos = {}, []
        for t in parts[2:]:
            if b"=" in t:
                k, v = t.split(b"=", 1); kv[k.decode("latin1")] = v.decode("latin1")
            elif t:
                pos.append(t.decode("latin1"))
        if cmd == "Load":
            self._load(int(kv["S"]), pos[0])
        elif cmd == "Queue":
            self._queue(int(kv["C"]), int(kv["S"]),
                        _apc_volume(kv["V"]) if "V" in kv else None)
        elif cmd == "Volume":
            self._volume(int(kv["C"]), _apc_volume(kv.get("V", "100")))
        elif cmd == "Flush":
            self._flush(int(kv["C"]))
        elif cmd == "Synth":
            self._synth(int(kv["S"]), kv.get("W", "SQ"),
                        float(kv.get("F", 440)), float(kv.get("T", 250)))
        elif cmd == "Update":
            self._update(int(kv["C"]))
        # Copy/Wait: not needed for the streaming path.

    def _update(self, chan):
        """A;Update;C=<ch>: arm the one-shot drain notification for the channel."""
        with self.lock:
            arm_drain(chan, self.channels, self.notify_arm, self.notify_last)

    # --- command impls -----------------------------------------------------
    def _store(self, name, data):
        with self.lock:
            self.cache[name] = data

    def _load(self, slot, name):
        with self.lock:
            data = self.cache.get(name)
        if data is None:
            return
        arr = _normalize(*_decode_audio(data))
        with self.lock:
            self.slots[slot] = arr

    def _queue(self, chan, slot, vol):
        with self.lock:
            arr = self.slots.get(slot)
            if arr is None:
                return
            ch = self.channels.setdefault(chan, _Channel())
            ch.queue.append(arr)
            if vol is not None:
                ch.volume = vol    # linear gain from _apc_volume()

    def _volume(self, chan, vol):
        with self.lock:
            self.channels.setdefault(chan, _Channel()).volume = vol

    def _flush(self, chan):
        with self.lock:
            ch = self.channels.get(chan)
            if ch:
                ch.queue.clear(); ch.pos = 0

    def _synth(self, slot, shape, hz, ms):
        n = int(DEVICE_RATE * ms / 1000.0)
        t = np.arange(n, dtype=np.float32) / DEVICE_RATE
        if shape == "SIN":
            w = np.sin(2 * np.pi * hz * t)
        elif shape == "SAW":
            w = 2 * (t * hz - np.floor(0.5 + t * hz))
        elif shape == "SILENCE":
            w = np.zeros(n, dtype=np.float32)
        else:  # SQ default
            w = np.sign(np.sin(2 * np.pi * hz * t))
        w = (w * 0.2).astype(np.float32)
        with self.lock:
            self.slots[slot] = np.column_stack([w, w])

    def frames_pending(self, chan):
        with self.lock:
            ch = self.channels.get(chan)
            return ch.frames_pending() if ch else 0

    def stop(self):
        stream, self.stream = self.stream, None
        if stream is not None:
            try:
                stream.stop()
            except Exception:
                pass
            try:
                stream.close()
            except Exception:
                pass


# --- WAV decode/encode + format normalize -----------------------------------
def _decode_audio(b):
    """Decode a cached clip: WAV via the stdlib, anything else (telnetvision
    can send FLAC when its producer has soundfile) via soundfile when
    available. Raises on undecodable data — the APC dispatcher logs it."""
    if b[:4] == b"RIFF":
        return _decode_wav(b)
    import soundfile  # optional; ImportError surfaces in the APC error log
    arr, fr = soundfile.read(io.BytesIO(b), dtype="float32", always_2d=True)
    return arr, fr, arr.shape[1]


def _decode_wav(b):
    w = wave.open(io.BytesIO(b))
    nf, ch, sw, fr = (w.getnframes(), w.getnchannels(),
                      w.getsampwidth(), w.getframerate())
    raw = w.readframes(nf); w.close()
    if sw == 2:
        a = np.frombuffer(raw, dtype="<i2").astype(np.float32) / 32768.0
    elif sw == 1:
        a = (np.frombuffer(raw, dtype=np.uint8).astype(np.float32) - 128) / 128.0
    else:
        a = np.frombuffer(raw, dtype="<i4").astype(np.float32) / 2147483648.0
    return a.reshape(-1, ch), fr, ch


def _normalize(arr, fr, ch):
    """-> contiguous float32 (n, 2) at DEVICE_RATE."""
    if ch == 1:
        arr = np.repeat(arr, 2, axis=1)
    elif ch > 2:
        arr = arr[:, :2]
    if fr != DEVICE_RATE and arr.shape[0] > 1:
        n = arr.shape[0]
        dst_n = int(round(n * DEVICE_RATE / fr))
        xo = np.linspace(0.0, 1.0, n, endpoint=False)
        xn = np.linspace(0.0, 1.0, dst_n, endpoint=False)
        arr = np.column_stack([np.interp(xn, xo, arr[:, 0]),
                               np.interp(xn, xo, arr[:, 1])])
    return np.ascontiguousarray(arr, dtype=np.float32)


def _encode_wav(arr, rate=DEVICE_RATE):
    """float32 (n,2) -> S16 stereo WAV bytes (what the door would Store)."""
    i16 = (np.clip(arr, -1.0, 1.0) * 32767.0).astype("<i2")
    buf = io.BytesIO()
    w = wave.open(buf, "wb")
    w.setnchannels(2); w.setsampwidth(2); w.setframerate(rate)
    w.writeframes(i16.tobytes()); w.close()
    return buf.getvalue()


# Lazy singleton so ANSI-music-only sessions never open the audio device.
_engine = None
_audio_retry_at = 0.0
_audio_last_error = None
AUDIO_RETRY_SEC = 1.0


def get_audio_engine():
    global _engine, _audio_retry_at, _audio_last_error
    if _engine is not None:
        if _engine.is_healthy():
            return _engine
        if time.monotonic() < _audio_retry_at:
            return None
        previous_status = _engine.callback_status
        try:
            _engine.reopen()
            sys.stderr.write("[BBSproxy] audio device reopened%s\r\n" %
                             ((": " + previous_status) if previous_status else ""))
            _audio_last_error = None
            return _engine
        except Exception as e:
            _audio_last_error = e
            _audio_retry_at = time.monotonic() + AUDIO_RETRY_SEC
            sys.stderr.write("[BBSproxy] audio device reopen failed; retrying: %r\r\n" % (e,))
            return None
    if time.monotonic() < _audio_retry_at:
        return None
    if not HAVE_AUDIO:
        sys.stderr.write("[BBSproxy] APC audio needs: pip install sounddevice numpy\r\n")
        _audio_retry_at = time.monotonic() + AUDIO_RETRY_SEC
        return None
    try:
        _engine = AudioEngine()
        _audio_last_error = None
    except Exception as e:
        _audio_last_error = e
        _audio_retry_at = time.monotonic() + AUDIO_RETRY_SEC
        sys.stderr.write("[BBSproxy] audio device init failed; retrying: %r\r\n" % (e,))
        return None
    return _engine


def shutdown_audio_engine():
    global _engine, _audio_retry_at, _audio_last_error
    if _engine is not None:
        _engine.stop()
    _engine = None
    _audio_retry_at = 0.0
    _audio_last_error = None


def _audio_dsr_reply(body):
    """Reply for a host audio-state DSR request. body is what follows
    'CSI =7', i.e. b'' (all channels) or b';<ch>' (one channel). Mirrors
    audio_apc.c: reply CSI =7 [; id ; state]... n, state 1=running 0=stopped.
    Uses the engine only if it already exists — a state poll must not open
    the audio device."""
    eng = _engine
    if body.startswith(b";"):
        try:
            ch = int(body[1:])
        except ValueError:
            return b""
        state = 1 if (eng is not None and eng.frames_pending(ch) > 0) else 0
        return b"\x1b[=7;%d;%dn" % (ch, state)
    pairs = bytearray()
    if eng is not None:
        with eng.lock:
            running = sorted(c for c, ch in eng.channels.items()
                             if ch.frames_pending() > 0)
        for c in running:
            pairs += b";%d;1" % c
    return b"\x1b[=7" + bytes(pairs) + b"n"


# ----------------------------------------------------------------------------
# Translator: VT state machine. Text bytes get CP437-mapped; music introducers
# divert to MML capture; ESC_ diverts to APC capture (SyncTERM audio).
# ----------------------------------------------------------------------------
class Translator:
    APC_MAX = 8 * 1024 * 1024

    def __init__(self, player=None, get_audio=None, bridge=None):
        self.player = player
        self.get_audio = get_audio
        self.bridge = bridge          # KeyboardBridge: intercept CTerm key control
        self.to_door = bytearray()    # replies (e.g. CTDA) to send back to the BBS
        self.state = "data"
        self.csi = bytearray()
        self.music = bytearray()
        self.apc = bytearray()
        self._mintro = b"\x1b[M"
        self._mstrip = False

    def feed(self, data):
        out = bytearray()
        i, n = 0, len(data)
        while i < n:
            # APC payload: bulk-consume to the next ESC (payloads are tens of KB).
            if self.state == "apc":
                j = data.find(0x1b, i)
                if j < 0:
                    self.apc += data[i:]; i = n
                else:
                    self.apc += data[i:j]; i = j + 1; self.state = "apc_esc"
                if len(self.apc) > self.APC_MAX:
                    self.state = "data"; self.apc = bytearray()
                continue

            b = data[i]; i += 1
            if self.state == "data":
                self._data_byte(b, out)
            elif self.state == "esc":
                if b == 0x5B:
                    self.state = "csi"; self.csi = bytearray(b"\x1b[")
                elif b == 0x5F:                 # ESC _  -> APC
                    self.state = "apc"; self.apc = bytearray()
                else:
                    out += b"\x1b"; out.append(b); self.state = "data"
            elif self.state == "csi":
                self.csi.append(b)
                if 0x40 <= b <= 0x7E:
                    c = bytes(self.csi)
                    if c == b"\x1b[M":
                        self.state = "music"; self.music = bytearray()
                        self._mintro = b"\x1b[M"; self._mstrip = False
                    elif b == 0x7C:
                        self.state = "music"; self.music = bytearray()
                        self._mintro = c; self._mstrip = True
                    elif b == 0x4E:
                        self.state = "music"; self.music = bytearray()
                        self._mintro = c; self._mstrip = False
                    elif b == 0x6E and c.startswith(b"\x1b[=7"):
                        # Audio state DSR request (CSI = 7 [; ch] n): the
                        # telnetvision door polls this every 250ms as its
                        # backpressure cross-check. Never forward to iTerm.
                        self.to_door += _audio_dsr_reply(c[4:-1])
                        self.state = "data"
                    elif self.bridge is not None:
                        fwd, reply = self.bridge.handle_door_csi(c)
                        out += fwd; self.to_door += reply
                        self.state = "data"
                    else:
                        out += self.csi; self.state = "data"
            elif self.state == "music":
                if self._mstrip:
                    self._mstrip = False
                    if b in (0x42, 0x46):
                        continue
                if b == 0x0E:
                    if self.player:
                        self.player.play(bytes(self.music).decode("latin1"))
                    self.state = "data"
                elif b == 0x1B or len(self.music) > 1024:
                    out += self._mintro + self.music
                    self.state = "data"; i -= 1
                else:
                    self.music.append(b)
            elif self.state == "apc_esc":
                if b == 0x5C:                   # ST = ESC \  -> APC complete
                    self._handle_apc(bytes(self.apc), out)
                    self.state = "data"; self.apc = bytearray()
                elif b == 0x1B:
                    self.apc.append(0x1B)       # literal ESC in payload; keep waiting
                else:
                    self.apc += b"\x1b"; self.apc.append(b); self.state = "apc"
        return bytes(out)

    def _handle_apc(self, payload, out):
        if payload.startswith(b"SyncTERM:Q;"):
            # Feature query (e.g. telnetvision's door probes Q;libsndfile at
            # session start and disables audio without this reply). Answer
            # from importability, NOT get_audio(): opening the device here
            # would grab it for sessions that never play a sound.
            feat = payload[len(b"SyncTERM:Q;"):]
            if feat == b"libsndfile":
                self.to_door += b"\x1b[=7;100;%dn" % (1 if HAVE_AUDIO else 0)
            return
        if payload.startswith(b"SyncTERM:"):
            eng = self.get_audio() if self.get_audio else None
            if eng is None:
                return                          # deps missing/disabled: drop it
            try:
                eng.handle_apc(payload[len(b"SyncTERM:"):])
            except Exception as e:
                sys.stderr.write("[BBSproxy] APC error: %r\r\n" % (e,))
        else:
            out += b"\x1b_" + payload + b"\x1b\\"   # not ours: pass through

    def _data_byte(self, b, out):
        if b == 0x1B:
            self.state = "esc"
        elif b in LIVE_CONTROLS:
            out.append(b)
        elif b == 0x00:
            pass
        elif 0x01 <= b <= 0x1F:
            out += CP437_LOW[b].encode("utf-8")
        elif 0x20 <= b <= 0x7E:
            out.append(b)
        else:
            out += bytes((b,)).decode("cp437").encode("utf-8")


# ----------------------------------------------------------------------------
# Legacy ANSI music: GW-BASIC PLAY MML -> square wave -> afplay (monophonic).
# ----------------------------------------------------------------------------
SAMPLE_RATE = 44100
AUDIO_CMD = "afplay"
C0 = 16.351597831287414
SEMI = {"C": 0, "D": 2, "E": 4, "F": 5, "G": 7, "A": 9, "B": 11}


def freq(idx):
    return C0 * (2 ** (idx / 12.0))


def parse_mml(s):
    s = s.upper()
    tempo, octave, deflen, gap = 120, 4, 4, 0.125
    i, n = 0, len(s)

    def read_int(j, default=None):
        start = j
        while j < n and s[j].isdigit():
            j += 1
        return (int(s[start:j]) if j > start else default), j

    def dur(length, dots):
        d = 240.0 / (tempo * length)
        total, add = d, d
        for _ in range(dots):
            add /= 2.0; total += add
        return total

    while i < n:
        c = s[i]; i += 1
        if c in " \t|":
            continue
        elif c == "T":
            v, i = read_int(i, tempo); tempo = max(32, v)
        elif c == "O":
            v, i = read_int(i, octave); octave = max(0, min(6, v))
        elif c == "L":
            v, i = read_int(i, deflen); deflen = max(1, v)
        elif c == ">":
            octave = min(6, octave + 1)
        elif c == "<":
            octave = max(0, octave - 1)
        elif c == "M":
            if i < n:
                gap = {"N": 0.125, "L": 0.0, "S": 0.25}.get(s[i], gap); i += 1
        elif c in SEMI:
            semi = SEMI[c]
            while i < n and s[i] in "#+-":
                semi += 1 if s[i] in "#+" else -1; i += 1
            length, i = read_int(i, deflen)
            dots = 0
            while i < n and s[i] == ".":
                dots += 1; i += 1
            d = dur(length, dots)
            f = freq(octave * 12 + semi)
            yield (f, d * (1 - gap))
            if gap:
                yield (0, d * gap)
        elif c in "PR":
            length, i = read_int(i, deflen)
            dots = 0
            while i < n and s[i] == ".":
                dots += 1; i += 1
            yield (0, dur(length, dots))
        elif c == "N":
            num, i = read_int(i, 0)
            length, i = read_int(i, deflen)
            d = dur(length, 0)
            yield ((0, d) if num == 0 else (freq(num), d))


def render(mml):
    amp = int(0.18 * 32767)
    frames = bytearray()
    for f, d in parse_mml(mml):
        count = int(SAMPLE_RATE * d)
        if f <= 0:
            frames += b"\x00\x00" * count
            continue
        period = SAMPLE_RATE / f
        fade = min(220, count // 4)
        for k in range(count):
            v = amp if (k % period) < (period / 2) else -amp
            if k < fade:
                v = int(v * k / fade)
            elif k > count - fade:
                v = int(v * (count - k) / fade)
            frames += struct.pack("<h", v)
    return bytes(frames)


class Player:
    """Monophonic MML player (PC-speaker semantics: newest sound wins)."""

    def __init__(self):
        self.cv = threading.Condition()
        self.pending = None
        self.current = None
        self.alive = True
        threading.Thread(target=self._worker, daemon=True).start()

    def play(self, mml):
        with self.cv:
            self.pending = mml
            if self.current and self.current.poll() is None:
                self.current.terminate()
            self.cv.notify()

    def stop(self):
        with self.cv:
            self.alive = False
            self.pending = None
            if self.current and self.current.poll() is None:
                self.current.terminate()
            self.cv.notify()

    def _worker(self):
        while True:
            with self.cv:
                while self.alive and self.pending is None:
                    self.cv.wait()
                if not self.alive:
                    return
                mml, self.pending = self.pending, None
            try:
                frames = render(mml)
                if not frames:
                    continue
                path = tempfile.mktemp(suffix=".wav")
                with wave.open(path, "wb") as w:
                    w.setnchannels(1); w.setsampwidth(2); w.setframerate(SAMPLE_RATE)
                    w.writeframes(frames)
                with self.cv:
                    if not self.alive or self.pending is not None:
                        os.unlink(path)
                        if not self.alive:
                            return
                        continue
                    self.current = subprocess.Popen(
                        [AUDIO_CMD, path],
                        stderr=subprocess.DEVNULL, stdout=subprocess.DEVNULL)
                    proc = self.current
                proc.wait()
                with self.cv:
                    self.current = None
                try:
                    os.unlink(path)
                except OSError:
                    pass
            except Exception:
                with self.cv:
                    self.current = None


# ----------------------------------------------------------------------------
# tty raw mode helpers
# ----------------------------------------------------------------------------
def _enter_raw():
    try:
        import termios, tty
        old = termios.tcgetattr(0); tty.setraw(0); return old
    except Exception:
        return None


def _restore_raw(old):
    if old is not None:
        import termios
        termios.tcsetattr(0, termios.TCSADRAIN, old)


# ----------------------------------------------------------------------------
# Mode 1: built-in telnet client
# ----------------------------------------------------------------------------
def run_telnet(host, port, cterm_bridge=False):
    player = Player(); tn = Telnet(); bridge = KeyboardBridge(cterm_bridge)
    tr = Translator(player, get_audio=get_audio_engine, bridge=bridge)
    sock = socket.create_connection((host, port))
    sock.setblocking(False)
    sock.sendall(bytes((IAC, DO, OPT_BINARY, IAC, WILL, OPT_BINARY,
                        IAC, DO, OPT_SGA, IAC, WILL, OPT_SGA,
                        IAC, WILL, OPT_TTYPE)))
    old = _enter_raw()
    try:
        while True:
            # Wake periodically even with no traffic, so drained-channel notifies
            # get flushed back to the door promptly (its underrun recovery).
            r, _, _ = select.select([sock, 0], [], [], 0.05)
            if sock in r:
                try:
                    data = sock.recv(8192)
                except BlockingIOError:
                    data = b""
                if data == b"":
                    break
                clean, reply = tn.feed(data)
                if reply:
                    sock.sendall(reply)
                out = tr.feed(clean)
                if out:
                    os.write(1, out)
                if tr.to_door:                 # CTDA reply etc. back to the BBS
                    sock.sendall(bytes(tr.to_door)); tr.to_door = bytearray()
            if 0 in r:
                kb = os.read(0, 4096)
                if not kb or b"\x1d" in kb:
                    break
                sock.sendall(bridge.from_terminal(kb))
            if _engine is not None:            # recover device changes + report drains
                eng = get_audio_engine()
                nd = eng.take_notify() if eng is not None else b""
                if nd:
                    sock.sendall(nd)
    finally:
        os.write(1, bridge.KITTY_OFF)          # restore iTerm's keyboard mode
        player.stop()
        shutdown_audio_engine()
        _restore_raw(old)
        sock.close()
        os.write(1, b"\r\n[disconnected]\r\n")


# ----------------------------------------------------------------------------
# Mode 2: wrap any command (ssh, rlogin, nc, telnet ...) in a pty
# ----------------------------------------------------------------------------
def run_exec(argv, cterm_bridge=False):
    import termios, fcntl, signal
    player = Player(); bridge = KeyboardBridge(cterm_bridge)
    tr = Translator(player, get_audio=get_audio_engine, bridge=bridge)

    pid, fd = pty.fork()
    if pid == 0:
        os.execvp(argv[0], argv)
        os._exit(127)

    def copy_winsize(*_):
        try:
            ws = fcntl.ioctl(0, termios.TIOCGWINSZ, b"\x00" * 8)
            fcntl.ioctl(fd, termios.TIOCSWINSZ, ws)
        except Exception:
            pass

    copy_winsize()
    try:
        signal.signal(signal.SIGWINCH, copy_winsize)
    except Exception:
        pass

    old = _enter_raw()
    try:
        while True:
            # Timeout so drained-channel notifies reach the door even when idle.
            r, _, _ = select.select([fd, 0], [], [], 0.05)
            if fd in r:
                try:
                    data = os.read(fd, 8192)
                except OSError:
                    data = b""
                if not data:
                    break
                out = tr.feed(data)
                if out:
                    os.write(1, out)
                if tr.to_door:                 # CTDA reply etc. back to the BBS
                    os.write(fd, bytes(tr.to_door)); tr.to_door = bytearray()
            if 0 in r:
                try:
                    kb = os.read(0, 4096)
                except OSError:
                    kb = b""
                if not kb:
                    break
                os.write(fd, bridge.from_terminal(kb))
            if _engine is not None:            # recover device changes + report drains
                eng = get_audio_engine()
                nd = eng.take_notify() if eng is not None else b""
                if nd:
                    os.write(fd, nd)
    finally:
        os.write(1, bridge.KITTY_OFF)          # restore iTerm's keyboard mode
        player.stop()
        shutdown_audio_engine()
        _restore_raw(old)
        try:
            os.close(fd)
        except OSError:
            pass
        try:
            os.waitpid(pid, 0)
        except OSError:
            pass
        os.write(1, b"\r\n[disconnected]\r\n")


# ----------------------------------------------------------------------------
# Mode 3: pure output filter for a pipe
# ----------------------------------------------------------------------------
def run_filter():
    player = Player(); tr = Translator(player, get_audio=get_audio_engine)
    try:
        while True:
            data = os.read(0, 8192)
            if not data:
                break
            out = tr.feed(data)
            if out:
                os.write(1, out)
    finally:
        player.stop()
        shutdown_audio_engine()


# ----------------------------------------------------------------------------
# --test-apc: prove gapless PCM streaming + the latency floor, no door needed.
# Chunks a source into clips, emits real Store/Load/Queue APCs, and feeds them
# through the SAME Translator+engine a remote door would drive. The first
# `prebuffer` chunks go out back-to-back; the rest are paced in real time, so
# end-to-end audio latency ~= prebuffer * chunk_ms (+ device latency).
# ----------------------------------------------------------------------------
def _demo_source(seconds=6.0):
    """A staccato arpeggio: any gap/click at a clip seam is obvious."""
    rate = DEVICE_RATE
    notes = [261.63, 329.63, 392.00, 523.25, 392.00, 329.63]
    spn = int(rate * 0.25)
    out = []
    for k in range(int(seconds / 0.25)):
        f = notes[k % len(notes)]
        t = np.arange(spn, dtype=np.float32) / rate
        env = np.minimum(1.0, np.minimum(t * 40, (0.25 - t) * 40)).astype(np.float32)
        w = (np.sign(np.sin(2 * np.pi * f * t)) * 0.18 * env).astype(np.float32)
        out.append(np.column_stack([w, w]))
    return np.ascontiguousarray(np.concatenate(out), dtype=np.float32)


def run_test_apc(path, chunk_ms, prebuffer):
    import time
    eng = get_audio_engine()
    if eng is None:
        sys.exit("APC audio unavailable (need: pip install sounddevice numpy)")
    tr = Translator(player=None, get_audio=get_audio_engine)

    if path:
        with open(path, "rb") as f:
            src = _normalize(*_decode_wav(f.read()))
    else:
        src = _demo_source()

    rate = DEVICE_RATE
    chunk = max(1, int(rate * chunk_ms / 1000.0))
    nchunks = (src.shape[0] + chunk - 1) // chunk
    chan, slots = 2, 8
    dev_lat = getattr(eng.stream, "latency", 0.0) or 0.0
    print("[test-apc] chunk=%dms prebuffer=%d  -> approx latency %.0fms "
          "(+%.0fms device)" % (chunk_ms, prebuffer,
                                prebuffer * chunk_ms, dev_lat * 1000.0))
    print("[test-apc] %d chunks; listen for clicks/gaps at clip seams." % nchunks)

    start = time.monotonic()
    for i in range(nchunks):
        piece = src[i * chunk:(i + 1) * chunk]
        name = "c%d" % (i % slots); slot = i % slots
        b64 = base64.b64encode(_encode_wav(piece, rate))
        apc = (b"\x1b_SyncTERM:C;S;" + name.encode() + b";" + b64 + b"\x1b\\"
               + b"\x1b_SyncTERM:A;Load;S=%d;%s\x1b\\" % (slot, name.encode())
               + b"\x1b_SyncTERM:A;Queue;C=%d;S=%d\x1b\\" % (chan, slot))
        target = start + max(0, (i - prebuffer)) * (chunk_ms / 1000.0)
        now = time.monotonic()
        if now < target:
            time.sleep(target - now)
        tr.feed(apc)

    while eng.frames_pending(chan) > 0:
        time.sleep(0.05)
    time.sleep(0.25)
    print("[test-apc] done.")
    shutdown_audio_engine()


# ----------------------------------------------------------------------------
def _test_kbd():
    """Isolation tests for both bridge modes."""
    # ---- DEFAULT: native kitty -- a transparent pipe both ways. -------------
    b = KeyboardBridge()
    for seq in (b"\x1b[<c", b"\x1b[?u", b"\x1b[c", b"\x1b[>10u", b"\x1b[<u",
                b"\x1b[=1h", b"\x1b[=2h"):
        assert b.handle_door_csi(seq) == (seq, b""), "native mode must forward %r" % seq
    for data in (b"hello\x1b[A", b"\x1b[?0u", b"\x1b[122;1:3u", b"\x1b[1;1:3C",
                 b"\x1b[24;80R"):
        assert b.from_terminal(data) == data, "native mode must pass %r" % data

    # ---- --cterm-bridge: impersonate SyncTERM. ------------------------------
    b = KeyboardBridge(cterm_bridge=True)
    # CTDA -> report physical keys (8); kitty query swallowed; DA1 forwarded.
    assert b.handle_door_csi(b"\x1b[<c") == (b"", b"\x1b[<0;8c")
    assert b.handle_door_csi(b"\x1b[?u") == (b"", b"")
    assert b.handle_door_csi(b"\x1b[c") == (b"\x1b[c", b"")
    # Legacy (pre-game): keys pass straight through unchanged.
    assert b.from_terminal(b"hello\x1b[A") == b"hello\x1b[A"
    # Door turns on physical keys -> enable kitty on the terminal + confirm query.
    assert b.handle_door_csi(b"\x1b[=1h") == (b"\x1b[>10u\x1b[?u", b"") and b.physical
    assert b.handle_door_csi(b"\x1b[=2h") == (b"", b"")

    # PHYSICAL mode, kitty not yet confirmed: nothing gets converted except
    # provable kitty forms; raw/legacy input must survive untouched.
    assert b.from_terminal(b"\x1b[24;80R") == b"\x1b[24;80R", "CPR must survive"
    assert b.from_terminal(b"z") == b"z", "raw letter must survive"
    for arrow in (b"\x1b[A", b"\x1b[B", b"\x1b[C", b"\x1b[D"):
        assert b.from_terminal(arrow) == arrow, "bare arrow pre-confirm must survive"
    # A real chunk: a CPR and a raw key arriving together still both survive.
    assert b.from_terminal(b"\x1b[24;80Rz") == b"\x1b[24;80Rz"

    # Kitty confirm: our query reply is swallowed and flips the flag.
    assert b.from_terminal(b"\x1b[?10u") == b"" and b.kitty_confirmed

    # Buttons -> CTerm evdev edges.
    assert b.from_terminal(b"\x1b[122u") == b"\x1b[=44K", "kitty z down -> A press"
    assert b.from_terminal(b"\x1b[122;1:2u") == b"", "kitty repeat ignored"
    assert b.from_terminal(b"\x1b[122;1:3u") == b"\x1b[=44k", "kitty z up -> A release"
    assert b.from_terminal(b"\x1b[13u") == b"\x1b[=28K", "kitty Enter -> Start"
    assert b.from_terminal(b"\x1b[27u") == b"\x1b[=1K", "kitty Esc -> evdev 1 (quit)"
    assert b.from_terminal(b"\x1b[112u") == b"\x1b[=25K", "kitty p -> palette"
    assert b.from_terminal(b"\x1b[97u") == b"\x1b[=30K", "kitty a -> accept"
    assert b.from_terminal(b"\x1b[97;1:3u") == b"\x1b[=30k"

    # Arrows -> CTerm evdev edges (the momentum fix): bare press converts once
    # kitty is confirmed; repeats drop; release is an up edge. Chording a button
    # mid-hold must not disturb the held direction.
    assert b.from_terminal(b"\x1b[C") == b"\x1b[=106K", "bare Right -> press edge"
    assert b.from_terminal(b"\x1b[1;1:2C") == b"", "arrow repeat dropped"
    assert b.from_terminal(b"\x1b[122u") == b"\x1b[=44K", "jump chord mid-hold"
    assert b.from_terminal(b"\x1b[122;1:3u") == b"\x1b[=44k"
    assert b.from_terminal(b"\x1b[1;1:3C") == b"\x1b[=106k", "Right release -> up edge"
    # Modifier form works too (Ctrl+Left press/release).
    assert b.from_terminal(b"\x1b[1;5D") == b"\x1b[=105K"
    assert b.from_terminal(b"\x1b[1;5:3D") == b"\x1b[=105k"

    # Door turns physical keys off -> disable kitty, back to straight passthrough.
    assert b.handle_door_csi(b"\x1b[=1l") == (b"\x1b[<u", b"") and not b.physical
    assert not b.kitty_confirmed
    assert b.from_terminal(b"abc\x1b[A") == b"abc\x1b[A"

    print("KeyboardBridge self-test: OK (native + cterm-bridge)")


def _test_volume():
    """Canonical volume semantics: percent and dB both map to linear gain."""
    assert _apc_volume("100") == 1.0
    assert abs(_apc_volume("50") - 0.5) < 1e-9
    assert _apc_volume("0") == 0.0
    assert _apc_volume("150") == 1.0, "percent clamps at 100"
    assert abs(_apc_volume("-6dB") - 10 ** (-0.3)) < 1e-6
    assert _apc_volume("0dB") == 1.0
    assert _apc_volume("garbage", default=1.0) == 1.0
    print("volume self-test: OK")


def _test_drain():
    """Isolation test of the door's feedback loop: after `A;Update;C=2` arms a
    channel, the shim must fire exactly one CSI=7;2;0n when that channel plays out
    and goes idle, stay silent until re-armed, and fire again after re-arming.
    Drives the real _Channel + poll logic, no audio device needed."""
    import numpy as np
    channels, arm, last = {}, {}, {}
    clip = np.zeros((100, 2), dtype=np.float32)
    out = np.zeros((100, 2), dtype=np.float32)

    # Door arms the one-shot before any audio is queued -> no spurious edge.
    arm_drain(2, channels, arm, last)
    assert poll_drain_notify(channels, arm, last) == b"", "fired before any audio"

    # A clip is queued: channel goes to 'playing' -> still no edge.
    ch = channels.setdefault(2, _Channel()); ch.queue.append(clip)
    assert poll_drain_notify(channels, arm, last) == b"", "fired while still playing"

    # Play it out; the FIFO goes idle -> exactly one notify on the play->idle edge.
    out.fill(0.0); ch.mix_into(out, 100)
    assert ch.frames_pending() == 0
    assert poll_drain_notify(channels, arm, last) == b"\x1b[=7;2;0n", "no drain notify"
    assert poll_drain_notify(channels, arm, last) == b"", "notify was not one-shot"

    # Door re-arms (its notify_drain path) and feeds again -> fires again.
    arm_drain(2, channels, arm, last)
    ch.queue.append(clip)
    assert poll_drain_notify(channels, arm, last) == b""       # playing again
    out.fill(0.0); ch.mix_into(out, 100)
    assert poll_drain_notify(channels, arm, last) == b"\x1b[=7;2;0n", "re-arm didn't fire"

    # Unarmed channels never notify, even when they drain.
    channels[3] = _Channel(); channels[3].queue.append(clip)
    channels[3].mix_into(np.zeros((100, 2), np.float32), 100)
    assert poll_drain_notify(channels, arm, last) == b"", "unarmed channel notified"
    print("drain-notify self-test: OK")


def _test_query():
    """Feature query + audio state DSR: the telnetvision door's detection
    handshake. It probes Q;libsndfile once at session start (no reply within
    ~500ms = audio disabled for the whole session) and polls channel state
    every 250ms. No audio device is opened by either."""
    global _engine
    tr = Translator(player=None, get_audio=None)

    # Feature probe -> one reply on the to_door channel, availability from deps.
    tr.feed(b"\x1b_SyncTERM:Q;libsndfile\x1b\\")
    want = b"\x1b[=7;100;%dn" % (1 if HAVE_AUDIO else 0)
    assert bytes(tr.to_door) == want, tr.to_door
    tr.to_door = bytearray()
    tr.feed(b"\x1b_SyncTERM:Q;bogus\x1b\\")
    assert bytes(tr.to_door) == b"", "unknown feature must stay silent"

    # State poll, no engine yet: stopped; the request never reaches iTerm.
    out = tr.feed(b"\x1b[=7;2n")
    assert out == b"" and bytes(tr.to_door) == b"\x1b[=7;2;0n", tr.to_door
    tr.to_door = bytearray()
    out = tr.feed(b"\x1b[=7n")
    assert out == b"" and bytes(tr.to_door) == b"\x1b[=7n", "all-idle reply"
    tr.to_door = bytearray()

    # With a playing channel the poll reports running (fake engine: the reply
    # builder must not instantiate one, only use what exists).
    class _Clip:  # anything with .shape[0]; keeps numpy out of this test
        shape = (100, 2)

    class _FakeEng:
        def __init__(self):
            self.lock = threading.Lock()
            self.channels = {2: _Channel()}
            self.channels[2].queue.append(_Clip())
        def frames_pending(self, ch):
            c = self.channels.get(ch)
            return c.frames_pending() if c else 0

    _engine = _FakeEng()
    try:
        tr.feed(b"\x1b[=7;2n")
        assert bytes(tr.to_door) == b"\x1b[=7;2;1n", tr.to_door
        tr.to_door = bytearray()
        tr.feed(b"\x1b[=7;5n")
        assert bytes(tr.to_door) == b"\x1b[=7;5;0n", "unopened channel = stopped"
        tr.to_door = bytearray()
        tr.feed(b"\x1b[=7n")
        assert bytes(tr.to_door) == b"\x1b[=7;2;1n", "all-channels lists running"
    finally:
        _engine = None
    print("feature-query/DSR self-test: OK")


def _test_audio_recovery():
    """Device loss recreates the PortAudio stream without losing mixer state."""
    global sd, HAVE_AUDIO, _engine, _audio_retry_at, _audio_last_error

    class FakeStream:
        def __init__(self, **kwargs):
            self.active = False
            self.closed = False
        def start(self):
            self.active = True
        def stop(self):
            self.active = False
        def close(self):
            self.closed = True

    class FakeSD:
        def __init__(self):
            self.fail = True
            self.streams = []
            self.device_name = "Speakers"
        def OutputStream(self, **kwargs):
            if self.fail:
                raise RuntimeError("temporary device loss")
            stream = FakeStream(**kwargs)
            self.streams.append(stream)
            return stream
        def query_devices(self, kind=None):
            return {"name": self.device_name, "hostapi": 0,
                    "max_output_channels": 2, "default_samplerate": 44100.0}

    old_sd, old_have = sd if "sd" in globals() else None, HAVE_AUDIO
    fake = FakeSD()
    shutdown_audio_engine()
    try:
        sd, HAVE_AUDIO = fake, True
        assert get_audio_engine() is None, "temporary init failure was not tolerated"
        fake.fail = False
        _audio_retry_at = 0.0              # simulate retry cooldown elapsing
        eng = get_audio_engine()
        assert eng is not None and eng.is_healthy(), "audio retry did not recover"

        clip = np.zeros((64, 2), dtype=np.float32)
        eng.channels[2] = _Channel()
        eng.channels[2].queue.append(clip)
        old_stream = eng.stream
        old_stream.active = False           # device disappeared/switched
        recovered = get_audio_engine()
        assert recovered is eng and recovered.is_healthy(), "inactive stream not reopened"
        assert recovered.stream is not old_stream and old_stream.closed
        assert recovered.frames_pending(2) == 64, "reopen discarded queued audio"

        # A default-device switch must reopen even while the old stream still
        # claims to be active (common when switching macOS output devices).
        old_stream = recovered.stream
        fake.device_name = "Headphones"
        recovered.last_device_check = 0.0
        recovered = get_audio_engine()
        assert recovered.stream is not old_stream and old_stream.closed, \
            "default output change did not reopen the active stream"
        assert recovered.frames_pending(2) == 64, "device switch lost queued audio"

        shutdown_audio_engine()
        assert _engine is None, "shutdown left a stale singleton"
    finally:
        shutdown_audio_engine()
        sd, HAVE_AUDIO = old_sd, old_have
    print("audio-recovery self-test: OK")


def main():
    a = sys.argv[1:]
    # --cterm-bridge: impersonate SyncTERM physical keys for the door instead of
    # the default transparent native-kitty negotiation (fallback / other doors).
    cterm_bridge = "--cterm-bridge" in a
    a = [x for x in a if x != "--cterm-bridge"]
    if not a:
        print(__doc__); sys.exit(1)
    if a[0] in ("--version", "-V"):
        print("BBSproxy apc (2026-06-26)"); return
    if a[0] == "--test-kbd":
        _test_kbd(); return
    if a[0] == "--test-drain":
        _test_drain(); return
    if a[0] == "--test-query":
        _test_query(); return
    if a[0] == "--test-volume":
        _test_volume(); return
    if a[0] == "--test-audio-recovery":
        _test_audio_recovery(); return
    if a[0] == "--test-music":
        Player().play(a[1] if len(a) > 1 else "T120O4L8CDEFGAB>C")
        import time; time.sleep(4); return
    if a[0] == "--test-apc":
        path, chunk_ms, prebuffer = None, 300, 2
        rest = a[1:]
        j = 0
        while j < len(rest):
            if rest[j] == "--chunk-ms" and j + 1 < len(rest):
                chunk_ms = int(rest[j + 1]); j += 2
            elif rest[j] == "--prebuffer" and j + 1 < len(rest):
                prebuffer = int(rest[j + 1]); j += 2
            else:
                path = rest[j]; j += 1
        run_test_apc(path, chunk_ms, prebuffer); return
    if a[0] == "--filter":
        run_filter(); return
    if a[0] == "--exec":
        if len(a) < 2:
            sys.exit("usage: --exec <command ...>")
        run_exec(a[1:], cterm_bridge); return
    if a[0] == "ssh":
        run_exec(a, cterm_bridge); return
    host = a[0]
    port = int(a[1]) if len(a) > 1 else 23
    run_telnet(host, port, cterm_bridge)


if __name__ == "__main__":
    main()
