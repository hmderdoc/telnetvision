#!/usr/bin/env python3
"""Capture (or synthesize) frames, optionally color-grade them, and push
full-color pixels to the fanout service. When run in a terminal it also shows a
live mirror with on-the-fly grading controls.

The wire carries full-color RGB at terminal resolution; the door decides how to
render it (truecolor vs 16-color). Connects OUT, so the capture host needs no
inbound ports.

Mirror keys (grading is applied before send, so the mirror = what callers get):
  +/-  saturation     [ ]  contrast     < >  brightness
  m    mirror render mode (half-block / ramp, LOCAL preview only)
  a    toggle the local audio monitor (only with --audio; macOS/Linux)
  0    reset grading      q  quit
Dimensions are fixed per session (set --cols/--rows); they are not live-editable
because changing them resizes every BBS caller's screen.
"""
import argparse
import collections
import os
import select
import shutil
import socket
import ssl
import struct
import subprocess
import sys
import threading
import time

# termios/tty are Unix-only. On Windows we just skip the interactive mirror.
try:
    import termios
    import tty
    _HAVE_TTY = True
except ImportError:
    termios = tty = None
    _HAVE_TTY = False

import cv2
import numpy as np

import ascii_cam
import wire


def connect(args):
    sock = socket.create_connection((args.host, args.port))
    # Audio chunks are small, frequent writes; Nagle (Python's default) can
    # hold each one for up to an RTT waiting to coalesce — audible as jitter
    # at the caller. Frames don't care; send everything immediately.
    sock.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
    # Keep the kernel send queue shallow: with the default (hundreds of KB)
    # a burst of frames can sit in the OS ahead of every audio chunk, adding
    # up to seconds of one-way audio delay on a slow uplink. 32KB still
    # sustains ~0.5MB/s at 60ms RTT — plenty for frames + audio.
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_SNDBUF, 32768)
    if args.tls:
        ctx = ssl.create_default_context()
        if args.insecure:
            ctx.check_hostname = False
            ctx.verify_mode = ssl.CERT_NONE
        sock = ctx.wrap_socket(sock, server_hostname=args.host)
    return sock


def parse_size(s):
    w, h = s.lower().split("x")
    return int(w), int(h)


def open_capture(source, camera):
    """cv2.VideoCapture for a device index, file path, or URL (rtsp/http/...)."""
    if source == "camera":
        target, is_device = camera, True
    elif source.isdigit():
        target, is_device = int(source), True  # USB cam, HDMI capture, OBS virtual cam
    else:
        target, is_device = source, False      # file path or stream URL
    cap = cv2.VideoCapture(target)
    if not cap.isOpened():
        raise SystemExit(f"cannot open source: {source!r}")
    # Cap the FFmpeg/HTTP backend's input buffer to a single frame so a slow
    # consumer doesn't let stream frames pile up. Skip for device captures
    # (webcam, HDMI card, OBS virtual cam): the AVFoundation/DSHOW/V4L2
    # backends don't accumulate that way, and on macOS AVFoundation a
    # BUFFERSIZE set on a capture device can stall the read pipeline.
    if not is_device:
        cap.set(cv2.CAP_PROP_BUFFERSIZE, 1)
    return cap


def test_frame(cols, rows, n):
    """Animated color field, sized (2*rows, cols, 3) BGR — no camera needed."""
    h, w = 2 * rows, cols
    xx, yy = np.meshgrid(np.arange(w), np.arange(h))
    r = np.sin((xx + n * 1.5) / 7.0) * 127 + 128
    g = np.sin((yy + n) / 6.0) * 127 + 128
    b = np.sin((xx + yy + n * 2.0) / 9.0) * 127 + 128
    return np.dstack([b, g, r]).astype(np.uint8)


def read_caption(path):
    """Return the current caption (last non-empty line) from an external feed
    file, as UTF-8 bytes. Reads only the tail, so an appended or overwritten file
    both work. Empty/missing path or file -> b''."""
    if not path:
        return b""
    try:
        with open(path, "rb") as f:
            f.seek(0, 2)
            f.seek(max(0, f.tell() - 512))
            tail = f.read()
    except OSError:
        return b""
    lines = [ln.strip() for ln in tail.decode("utf-8", "replace").splitlines() if ln.strip()]
    return (lines[-1] if lines else "")[:240].encode("utf-8", "replace")


class Diag:
    """Per-phase timing + a watchdog that fires the moment any phase blocks
    longer than `stuck_after` seconds. Only created when --diag-file is set;
    when unset, the NullDiag below makes the call sites no-ops.

    Heartbeat every `heartbeat` seconds: one line with frames-per-window, fps,
    and the max wall-clock duration observed for each phase that ran in the
    window. Watchdog runs in a daemon thread checking the current phase every
    0.5 s — if a freeze happens, the *last* line of the diag file names the
    phase and how long it's been stuck."""

    def __init__(self, path, stuck_after=3.0, heartbeat=2.0):
        self.f = open(path, "a", buffering=1)
        self.phase = "init"
        self.phase_t = time.monotonic()
        self.frame_count = 0
        self.phase_max = {}
        self.last_hb = time.monotonic()
        self.last_alert = ("", 0.0)
        self.stuck_after = stuck_after
        self.heartbeat = heartbeat
        self._log(f"diag start  pid={os.getpid()}")
        threading.Thread(target=self._watchdog, daemon=True).start()

    def _log(self, line):
        try:
            self.f.write(f"[{time.strftime('%H:%M:%S')}] {line}\n")
        except OSError:
            pass

    def set_phase(self, name):
        now = time.monotonic()
        prev_dt = now - self.phase_t
        if self.phase != "init":
            cur = self.phase_max.get(self.phase, 0.0)
            if prev_dt > cur:
                self.phase_max[self.phase] = prev_dt
        self.phase = name
        self.phase_t = now

    def frame_done(self):
        self.frame_count += 1
        now = time.monotonic()
        if now - self.last_hb < self.heartbeat:
            return
        maxes = self.phase_max
        self.phase_max = {}
        secs = now - self.last_hb
        fps = self.frame_count / secs
        self.frame_count = 0
        self.last_hb = now
        parts = " ".join(f"{k}={v * 1000:.1f}ms" for k, v in sorted(maxes.items()))
        self._log(f"fps={fps:.1f} max {parts}")

    def _watchdog(self):
        while True:
            time.sleep(0.5)
            cur, started = self.phase, self.phase_t
            stuck = time.monotonic() - started
            if stuck < self.stuck_after or cur in ("init", "idle"):
                continue
            # Avoid spamming: re-alert only after another stuck_after seconds.
            if self.last_alert[0] == cur and stuck - self.last_alert[1] < self.stuck_after:
                continue
            self._log(f"STUCK in phase {cur!r} for {stuck:.1f}s")
            self.last_alert = (cur, stuck)


class _NullDiag:
    def set_phase(self, name): pass
    def frame_done(self): pass


def grade(bgr, brightness, contrast, saturation):
    """Color-grade before send. brightness: additive; contrast/saturation: gain."""
    if contrast == 1.0 and brightness == 0 and saturation == 1.0:
        return bgr
    img = bgr.astype(np.float32)
    if contrast != 1.0:
        img = (img - 128.0) * contrast + 128.0
    if brightness != 0:
        img += brightness
    img = np.clip(img, 0, 255).astype(np.uint8)
    if saturation != 1.0:
        hsv = cv2.cvtColor(img, cv2.COLOR_BGR2HSV).astype(np.float32)
        hsv[..., 1] = np.clip(hsv[..., 1] * saturation, 0, 255)
        img = cv2.cvtColor(hsv.astype(np.uint8), cv2.COLOR_HSV2BGR)
    return img


# ---- audio streaming ---------------------------------------------------------

def wav_encode(pcm: bytes, rate: int, channels: int) -> bytes:
    """Minimal canonical RIFF/WAVE header around raw S16LE PCM. Pure stdlib,
    so codec=wav never needs an extra dependency."""
    byte_rate = rate * channels * 2
    return b"".join([
        b"RIFF", struct.pack("<I", 36 + len(pcm)), b"WAVE",
        b"fmt ", struct.pack("<IHHIIHH", 16, 1, channels, rate, byte_rate,
                             channels * 2, 16),
        b"data", struct.pack("<I", len(pcm)),
        pcm,
    ])


def flac_encoder():
    """Return a per-chunk FLAC encoder callable, or None if the optional
    `soundfile` package (libsndfile) isn't installed."""
    try:
        import io
        import soundfile  # type: ignore
    except ImportError:
        return None

    def encode(pcm: bytes, rate: int, channels: int) -> bytes:
        data = np.frombuffer(pcm, np.int16).reshape(-1, channels)
        buf = io.BytesIO()
        soundfile.write(buf, data, rate, format="FLAC")
        return buf.getvalue()

    return encode


def _reenumerate_devices(sd):
    """Rebuild PortAudio's device list. It snapshots the host's devices at
    init, so after a device is unplugged or re-enumerated (USB reset, sleep)
    the cached entry still resolves — and reopening binds straight back to the
    dead device object. Only a terminate/initialize cycle refreshes it."""
    try:
        sd._terminate()
        sd._initialize()
    except Exception as e:
        sys.stderr.write(f"audio: device re-enumeration failed ({e})\n")


def default_mic_args():
    """ffmpeg input args for the OS default microphone, or None when there is
    no guessable default (Windows dshow needs a device name — set
    AUDIO_SOURCE explicitly there)."""
    if sys.platform == "darwin":
        return ["-f", "avfoundation", "-i", ":0"]
    if sys.platform.startswith("linux"):
        return ["-f", "pulse", "-i", "default"]
    return None


def resolve_audio_capture(audio_source, video_source):
    """Decide HOW to capture audio: ('device', name_or_None) for a local
    input device via PortAudio, or ('ffmpeg', input_args) for a URL/file
    demux.

    Devices go through sounddevice/PortAudio because ffmpeg's audio-only
    avfoundation capture underdelivers (measured 0.42x realtime on macOS —
    audio produced at half speed plays as constant chop). PortAudio is paced
    by the device clock and sample-accurate. URLs and files keep ffmpeg,
    which demuxes those at realtime correctly.

    auto ('' / unset): a URL/file video source is demuxed for its own audio
    track; a device/test/stdin source falls back to the default mic.
    'mic': the default input device. ':<index>' or ':<name substring>': a
    specific input device. Anything else is an ffmpeg input (URL/file);
    local files loop at realtime (-re) to mirror the video path."""
    src = audio_source
    if src in ("", "auto"):
        is_av_source = (video_source not in ("camera", "test", "-")
                        and not video_source.isdigit())
        src = video_source if is_av_source else "mic"
    if src == "mic":
        return "device", None
    if src.startswith(":"):
        return "device", src[1:]
    args = []
    if os.path.isfile(src):
        args += ["-re", "-stream_loop", "-1"]
    return "ffmpeg", args + ["-i", src]


class Sender(threading.Thread):
    """The single socket writer. Audio outranks video: chunks queue in order
    (bounded, drop-oldest), while only the NEWEST unsent frame is kept — a
    slow uplink then sheds video quality instead of delaying sound, the same
    drop-to-latest contract the relay and the door already honor. Send
    errors are surfaced to the main loop (which owns reconnect) rather than
    handled here."""

    def __init__(self, sock_holder, send_lock):
        super().__init__(daemon=True)
        self.cv = threading.Condition()
        self.audio = collections.deque()
        self.video = None
        self.err = None
        self.sock_holder = sock_holder
        self.send_lock = send_lock

    def send_audio(self, payload):
        with self.cv:
            self.audio.append(payload)
            if len(self.audio) > 32:
                self.audio.popleft()
            self.cv.notify()

    def send_video(self, payload):
        with self.cv:
            self.video = payload  # replace any unsent frame (drop-to-latest)
            self.cv.notify()

    def take_error(self):
        with self.cv:
            e, self.err = self.err, None
            return e

    def run(self):
        while True:
            with self.cv:
                while not self.audio and self.video is None:
                    self.cv.wait()
                if self.audio:
                    payload = self.audio.popleft()
                else:
                    payload, self.video = self.video, None
            sock = self.sock_holder[0]
            if sock is None:
                time.sleep(0.05)  # reconnect in flight; don't spin on the GIL
                continue
            try:
                with self.send_lock:
                    # The main loop can rotate the socket while we wait for the
                    # lock; the payload it belongs to is already stale.
                    if self.sock_holder[0] is not sock:
                        continue
                    wire.send_msg(sock, payload)
            except (TimeoutError, OSError) as e:
                # Only a failure of the *live* socket should trigger a
                # reconnect. A socket the main loop already replaced was shut
                # down on purpose, and reporting that would reconnect twice.
                if self.sock_holder[0] is sock:
                    with self.cv:
                        self.err = e
                time.sleep(0.2)  # the main loop reconnects; don't spin


class AudioStreamer(threading.Thread):
    """Demux/capture audio with ffmpeg, chop it into chunks, encode, and send
    AUDIO wire messages stamped on the same monotonic clock as video frames.

    pts is sample-count-derived from an anchor taken at the first chunk, so
    consecutive chunks tile the timeline exactly; the anchor snaps back to the
    wall clock if the two drift apart (device hiccup, ffmpeg restart)."""

    def __init__(self, args, ctl, sender, t0):
        super().__init__(daemon=True)
        self.args = args
        self.ctl = ctl
        self.sender = sender
        self.t0 = t0
        self.codec_name = args.audio_codec
        self.flac = flac_encoder()
        if self.codec_name == "auto":
            self.codec_name = "flac" if self.flac else "wav"
        if self.codec_name == "flac" and not self.flac:
            sys.stderr.write("audio: soundfile not installed; falling back to wav "
                             "(pip install soundfile for FLAC)\n")
            self.codec_name = "wav"
        self.chunk_ms = min(200, max(40, args.audio_chunk_ms))
        self.monitor = None  # lazily-spawned ffplay for the local monitor
        self._anchor = None  # pts anchor (ms on the shared clock)
        self._samples = 0    # samples emitted since the anchor
        self._seq = 0

    def _spawn_ffmpeg(self, inp):
        cmd = ([os.environ.get("FFMPEG", "ffmpeg"),
                "-hide_banner", "-loglevel", "error", "-nostdin"]
               + inp
               + ["-vn", "-f", "s16le",
                  "-ar", str(self.args.audio_rate),
                  "-ac", str(self.args.audio_channels), "-"])
        try:
            return subprocess.Popen(cmd, stdout=subprocess.PIPE,
                                    stderr=subprocess.DEVNULL)
        except OSError as e:
            sys.stderr.write(f"audio: cannot run ffmpeg ({e}); audio disabled\n")
            return None

    def _monitor_write(self, pcm):
        """Local audio monitor (macOS/Linux): a persistent ffplay fed raw PCM,
        toggled live with the 'a' key. Starts muted — monitoring a mic input
        would feed back. Any failure just disables the monitor."""
        if not self.ctl.get("amon"):
            return
        if self.monitor is None:
            if sys.platform.startswith("win") or not shutil.which("ffplay"):
                self.ctl["amon"] = False
                return
            try:
                self.monitor = subprocess.Popen(
                    ["ffplay", "-nodisp", "-loglevel", "quiet", "-f", "s16le",
                     "-ar", str(self.args.audio_rate),
                     "-ac", str(self.args.audio_channels), "-i", "-"],
                    stdin=subprocess.PIPE, stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL)
            except OSError:
                self.ctl["amon"] = False
                return
        try:
            self.monitor.stdin.write(pcm)
            self.monitor.stdin.flush()
        except OSError:
            self.ctl["amon"] = False
            self.monitor = None

    def _emit(self, pcm):
        """Stamp one PCM chunk on the shared clock, encode, and hand it to
        the priority sender. Shared by both capture paths."""
        rate, channels = self.args.audio_rate, self.args.audio_channels
        now_ms = int((time.monotonic() - self.t0) * 1000)
        # Anchor at the chunk's capture START (clamped: the first read can
        # complete within chunk_ms of t0, and pts is unsigned).
        if self._anchor is None:
            self._anchor = max(0, now_ms - self.chunk_ms)
            self._samples = 0
        pts = self._anchor + self._samples * 1000 // rate
        # Sample-count pts drifting >1s behind the wall clock means the
        # capture stalled — snap the anchor forward.
        if now_ms - pts > 1000:
            self._anchor = max(0, now_ms - self.chunk_ms)
            self._samples = 0
            pts = self._anchor
        # Capture can never be from the future: ffmpeg primes live URLs and
        # -re files with an initial burst, which would stamp those chunks
        # ahead of the shared clock and make the door's sync gate drop
        # video. Clamp to "now" (still monotonic — both inputs only grow).
        pts = min(pts, now_ms)
        pts = max(0, pts + self.args.audio_offset_ms)
        self._samples += len(pcm) // (2 * channels)
        dur = len(pcm) * 1000 // (rate * channels * 2)

        if self.codec_name == "flac":
            payload = self.flac(pcm, rate, channels)
            codec_id = wire.AUDIO_CODEC_FLAC
        else:
            payload = wav_encode(pcm, rate, channels)
            codec_id = wire.AUDIO_CODEC_WAV
        self.sender.send_audio(wire.audio_payload(
            codec_id, channels, rate, self._seq, pts, dur, payload))
        self._seq += 1
        self._monitor_write(pcm)

    def _open_input(self, sd, frames, name):
        """Open and start a blocking input stream, or return None when a named
        device isn't present. Open failures raise (the caller retries)."""
        dev = None
        if name:
            if name.isdigit():
                dev = int(name)
            else:
                low = name.lower()
                for i, d in enumerate(sd.query_devices()):
                    if low in d["name"].lower() and d["max_input_channels"] > 0:
                        dev = i
                        break
                if dev is None:
                    return None
        stream = sd.RawInputStream(
            device=dev, samplerate=self.args.audio_rate,
            channels=self.args.audio_channels, dtype="int16", blocksize=frames)
        stream.start()
        return stream

    def _run_device(self, name):
        """Capture a local input device via PortAudio. Returns False when
        sounddevice isn't installed (caller falls back to ffmpeg with a
        warning) and True when the path was taken (even if it then failed —
        a broken device shouldn't silently switch inputs).

        Reads block on PortAudio's own ring buffer rather than being pushed
        from a Python callback. A Python callback runs *on* CoreAudio's
        realtime IOProc thread, where it has to wait for the GIL: that
        overruns the I/O deadline, macOS invalidates the device object, and
        every later cycle logs '||PaMacCore|| err=!obj' with the audio dead
        for the rest of the run. Blocking reads keep Python off that thread.
        """
        try:
            import sounddevice as sd
        except ImportError:
            sys.stderr.write(
                "audio: sounddevice not installed — falling back to ffmpeg "
                "capture, which is NOT rate-accurate on macOS "
                "(pip install sounddevice)\n")
            return False
        frames = max(1, self.args.audio_rate * self.chunk_ms // 1000)
        backoff = 0.5
        streamed = False  # a device that has worked before is worth waiting for
        attempts = 0
        while not self.ctl["quit"]:
            try:
                stream = self._open_input(sd, frames, name)
            except Exception as e:
                why = str(e)  # device busy, unsupported rate, gone mid-stream...
            else:
                # A name that matches nothing is a config error, never a
                # transient one — retrying it would just spin forever.
                if stream is None:
                    sys.stderr.write(
                        f"audio: no input device matching {name!r}; "
                        "audio disabled\n")
                    return True
                streamed, backoff, attempts = True, 0.5, 0
                why = None
            if why is not None:
                # Give up only if the device has never worked; once it has, a
                # failure means it dropped off the bus (USB reset, sleep) and
                # it usually comes back — wait for it however long that takes.
                attempts += 1
                if not streamed and attempts >= 5:
                    sys.stderr.write(
                        f"audio: cannot open input device ({why}); audio disabled\n")
                    return True
                sys.stderr.write(f"audio: cannot open input device ({why}); "
                                 f"retrying in {backoff:.1f}s\n")
                time.sleep(backoff)
                backoff = min(backoff * 2, 10.0)
                _reenumerate_devices(sd)
                continue
            try:
                while not self.ctl["quit"]:
                    # Blocks until a full chunk is captured. `overflowed` means
                    # we fell behind and PortAudio dropped input; _emit()
                    # re-anchors pts once the gap exceeds its drift window.
                    pcm, _overflowed = stream.read(frames)
                    self._emit(bytes(pcm))
            except Exception as e:
                sys.stderr.write(f"audio: input device lost ({e}); reopening\n")
            finally:
                try:
                    stream.stop()
                    stream.close()
                except Exception:
                    pass
            if self.ctl["quit"]:
                break
            # A device that disappeared mid-stream is still in PortAudio's
            # cached list, so reopening without re-enumerating rebinds to the
            # same dead device object.
            _reenumerate_devices(sd)
            self._anchor = None  # capture gap: pts must re-anchor
        return True

    def _run_ffmpeg(self, inp):
        """Demux a URL/file with ffmpeg (realtime for those inputs)."""
        rate, channels = self.args.audio_rate, self.args.audio_channels
        chunk_bytes = rate * channels * 2 * self.chunk_ms // 1000
        backoff = 1.0
        while not self.ctl["quit"]:
            proc = self._spawn_ffmpeg(inp)
            if proc is None:
                return
            while not self.ctl["quit"]:
                pcm = proc.stdout.read(chunk_bytes)
                if not pcm or len(pcm) < chunk_bytes:
                    break  # source ended or ffmpeg died; respawn below
                backoff = 1.0
                self._emit(pcm)
            proc.kill()
            if not self.ctl["quit"]:
                time.sleep(backoff)  # source hiccup: retry with backoff
                backoff = min(backoff * 2, 10.0)

    def run(self):
        kind, spec = resolve_audio_capture(self.args.audio_source, self.args.source)
        if kind == "device":
            if not self._run_device(spec):
                # sounddevice missing: ffmpeg fallback for whatever this
                # platform can address (macOS by :name, else default mic).
                inp = (["-f", "avfoundation", "-i", ":" + spec]
                       if spec and sys.platform == "darwin" else default_mic_args())
                if inp is None:
                    sys.stderr.write("audio: no capture path available; audio disabled\n")
                else:
                    self._run_ffmpeg(inp)
        else:
            self._run_ffmpeg(spec)
        if self.monitor is not None:
            try:
                self.monitor.kill()
            except OSError:
                pass


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--host", default="127.0.0.1")
    ap.add_argument("--port", type=int, default=7600)
    ap.add_argument("--channel", default="cam")
    ap.add_argument("--token", default="")
    ap.add_argument("--tls", action="store_true", help="wrap connection in TLS")
    ap.add_argument("--insecure", action="store_true", help="skip TLS verification")
    ap.add_argument("--source", default="camera",
                    help="camera | test | - (raw stdin) | <index> | <file or URL>")
    ap.add_argument("--camera", type=int, default=0, help="device index when --source camera")
    ap.add_argument("--in-size", default="640x480", help="WxH of bgr24 frames for --source -")
    ap.add_argument("--flip", action=argparse.BooleanOptionalAction, default=None,
                    help="mirror horizontally (default: auto — only --source camera)")
    ap.add_argument("--cols", type=int, default=80)
    ap.add_argument("--rows", type=int, default=25, help="cell rows (pixels = 2x)")
    ap.add_argument("--fps", type=float, default=15.0)
    ap.add_argument("--saturation", type=float, default=1.0, help="initial saturation (live: +/-)")
    ap.add_argument("--contrast", type=float, default=1.0, help="initial contrast (live: [ ])")
    ap.add_argument("--brightness", type=int, default=0, help="initial brightness (live: < >)")
    ap.add_argument("--mode", choices=["half", "ramp"], default="half",
                    help="initial render mode for callers (live: m)")
    ap.add_argument("--ramp", choices=["ascii", "shades"], default="ascii",
                    help="initial ramp glyphs when mode=ramp (live: g)")
    ap.add_argument("--caption-file", default="",
                    help="file whose last line is broadcast as a subtitle (external feed)")
    ap.add_argument("--audio", action=argparse.BooleanOptionalAction, default=False,
                    help="capture and stream audio alongside video (SyncTERM >= 1.10a callers)")
    ap.add_argument("--audio-source", default="",
                    help="auto (URL/file source's own track, else default mic) | mic | <ffmpeg input>")
    ap.add_argument("--audio-rate", type=int, default=44100)
    ap.add_argument("--audio-channels", type=int, default=2)
    ap.add_argument("--audio-codec", choices=["auto", "flac", "wav"], default="auto",
                    help="chunk container; flac halves bandwidth but needs `pip install soundfile`")
    ap.add_argument("--audio-chunk-ms", type=int, default=80,
                    help="audio chunk size, clamped to 40..200 (smaller = lower latency, more overhead)")
    ap.add_argument("--audio-offset-ms", type=int, default=0,
                    help="lip-sync trim: positive shifts audio earlier relative to video "
                         "(use if you hear words after seeing lips move)")
    ap.add_argument("--mirror", action=argparse.BooleanOptionalAction, default=True,
                    help="live local preview + key controls (auto-off if not a TTY)")
    ap.add_argument("--frames", type=int, default=0, help="stop after N frames (0=forever)")
    ap.add_argument("--diag-file", default="",
                    help="write per-phase timings here every 2s; a watchdog logs "
                         "'STUCK in phase X for Ys' if any phase blocks > 3s")
    ap.add_argument("--send-timeout", type=float, default=15.0,
                    help="seconds before a stalled sendall() fails with TimeoutError, "
                         "which triggers a close+reconnect (NOT a process exit). "
                         "0 disables — sendall can then block forever. Default 15s.")
    ap.add_argument("--reconnect-max-delay", type=float, default=30.0,
                    help="exponential backoff cap while reconnecting (s, default 30)")
    args = ap.parse_args()
    diag = Diag(args.diag_file) if args.diag_file else _NullDiag()

    source = args.source
    use_test = source == "test"
    use_stdin = source == "-"
    # stdin carries the video for "-", so it can't also drive the mirror keys.
    interactive = (args.mirror and not use_stdin and _HAVE_TTY
                   and sys.stdin.isatty() and sys.stdout.isatty())
    # Auto-mirror only the default selfie webcam. Explicit devices (incl. HDMI
    # capture cards by index), files, URLs and pipes keep their real orientation.
    do_flip = (source == "camera") if args.flip is None else args.flip

    cap = None
    stdin_buf = None
    in_shape = None
    frame_bytes = 0
    if use_stdin:
        in_w, in_h = parse_size(args.in_size)
        in_shape = (in_h, in_w, 3)
        frame_bytes = in_w * in_h * 3
        stdin_buf = sys.stdin.buffer
    elif not use_test:
        cap = open_capture(source, args.camera)

    # Live-adjustable state, shared with the stdin reader thread. Python's GIL
    # makes single-key writes/reads atomic; we snapshot at the top of each frame.
    # Defined before connect so 'q' during startup-retry interrupts cleanly.
    ctl = {"sat": args.saturation, "con": args.contrast, "bri": args.brightness,
           "mode": args.mode, "ramp": args.ramp, "amon": False, "quit": False}
    sock_holder = [None]  # the stdin reader uses this to find the current sock
    # One socket, two sender threads (video main loop + audio): serialize
    # sendall() so wire messages can't interleave mid-frame.
    send_lock = threading.Lock()
    # The shared A/V clock: every FRAME and AUDIO pts is milliseconds since
    # this instant, so the door can align the two streams. (Named clock0: the
    # frame loop below reuses t0 as its per-iteration timer.)
    clock0 = time.monotonic()

    def establish():
        """Open a fresh socket + send the producer hello. Raises on any
        failure (connect refused, TLS handshake error, hello write failure).
        connect_with_backoff() retries this."""
        s = connect(args)
        if args.send_timeout > 0:
            s.settimeout(args.send_timeout)
        caps = wire.CAP_AUDIO if args.audio else 0
        wire.hello_producer(s, args.token, args.channel, caps)
        return s

    def connect_with_backoff(label):
        """Try establish() until success or ctl["quit"]. Returns the new socket
        or None if the user quit during retry. Used for both initial startup
        and mid-stream reconnect."""
        delay = 1.0
        while not ctl["quit"]:
            try:
                return establish()
            except (OSError, TimeoutError) as e:
                sys.stderr.write(
                    f"producer: {label} failed ({e}); retrying in {delay:.0f}s\n")
                time.sleep(delay)
                delay = min(delay * 2, args.reconnect_max_delay)
        return None

    out = sys.stdout
    old_term = None
    if interactive:
        old_term = termios.tcgetattr(sys.stdin.fileno())
        tty.setcbreak(sys.stdin.fileno())
        out.write("\033[?25l\033[?7l\033[2J")  # hide cursor, disable auto-wrap, clear
        out.flush()

        # Background stdin reader: keeps 'q' responsive even when the main loop
        # is blocked in sendall(). On quit, we also shutdown the socket so a
        # blocked send wakes up immediately with OSError instead of waiting out
        # the send_timeout.
        def stdin_reader():
            while not ctl["quit"]:
                try:
                    chunk = os.read(sys.stdin.fileno(), 64)
                except OSError:
                    return
                if not chunk:
                    return
                for k in chunk.decode("latin-1", "ignore"):
                    if k in ("+", "="):   ctl["sat"] = round(min(3.0, ctl["sat"] + 0.1), 2)
                    elif k in ("-", "_"): ctl["sat"] = round(max(0.0, ctl["sat"] - 0.1), 2)
                    elif k == "]":        ctl["con"] = round(min(3.0, ctl["con"] + 0.1), 2)
                    elif k == "[":        ctl["con"] = round(max(0.1, ctl["con"] - 0.1), 2)
                    elif k in (".", ">"): ctl["bri"] = min(128, ctl["bri"] + 8)
                    elif k in (",", "<"): ctl["bri"] = max(-128, ctl["bri"] - 8)
                    elif k == "m":        ctl["mode"] = "ramp" if ctl["mode"] == "half" else "half"
                    elif k == "g":        ctl["ramp"] = "shades" if ctl["ramp"] == "ascii" else "ascii"
                    elif k == "0":        ctl["sat"], ctl["con"], ctl["bri"] = 1.0, 1.0, 0
                    elif k == "a":        ctl["amon"] = not ctl["amon"]
                    elif k in ("q", "Q"):
                        ctl["quit"] = True
                        s = sock_holder[0]
                        if s is not None:  # may not yet exist during startup retry
                            try:
                                s.shutdown(socket.SHUT_RDWR)
                            except OSError:
                                pass
                        return

        threading.Thread(target=stdin_reader, daemon=True).start()

    # Initial connect, with the same retry/backoff loop the mid-stream
    # reconnect uses. So a transient TLS handshake reset (or the cloud box
    # being down for a few seconds at boot) doesn't crash the producer — it
    # waits and tries again.
    sock = connect_with_backoff("connect")
    if sock is None:
        sys.exit(0)  # user pressed q during startup retry
    sock_holder[0] = sock

    sender = Sender(sock_holder, send_lock)
    sender.start()
    if args.audio:
        AudioStreamer(args, ctl, sender, clock0).start()

    interval = 1.0 / args.fps
    n = 0
    fails = 0
    try:
        while not ctl["quit"]:
            t0 = time.monotonic()
            sat, con, bri = ctl["sat"], ctl["con"], ctl["bri"]
            mode, ramp = ctl["mode"], ctl["ramp"]

            diag.set_phase("acquire")
            if use_test:
                frame = test_frame(args.cols, args.rows, n)
            elif use_stdin:
                raw = stdin_buf.read(frame_bytes)
                if len(raw) < frame_bytes:
                    break  # piped source ended
                frame = np.frombuffer(raw, np.uint8).reshape(in_shape)
            else:
                ok, frame = cap.read()
                if not ok:
                    cap.set(cv2.CAP_PROP_POS_FRAMES, 0)  # loop a file; no-op for live
                    ok, frame = cap.read()
                    if not ok:
                        fails += 1
                        if fails > 30:
                            break
                        continue
                fails = 0
            diag.set_phase("process")
            if do_flip:
                frame = cv2.flip(frame, 1)

            graded = grade(frame, bri, con, sat)
            small = cv2.resize(
                graded, (args.cols, 2 * args.rows), interpolation=cv2.INTER_AREA
            )

            diag.set_phase("read_caption")
            caption = read_caption(args.caption_file)

            # Send full-color downscaled pixels (BGR->RGB) plus the render
            # directive and any caption, so callers see what you chose.
            diag.set_phase("send")
            mode_i = 1 if mode == "ramp" else 0
            ramp_i = 1 if ramp == "shades" else 0
            pixels = np.ascontiguousarray(small[..., ::-1])
            pts_ms = int((time.monotonic() - clock0) * 1000)
            # Hand the frame to the priority sender: it never blocks here, and
            # an unsent frame is simply replaced by this fresher one while
            # audio chunks keep flowing ahead of both.
            sender.send_video(wire.frame_payload(
                args.cols, args.rows, pixels.tobytes(), mode_i, ramp_i,
                caption, pts_ms))
            e = sender.take_error()
            if e is not None:
                if ctl["quit"]:
                    break  # user pressed q; the stdin thread shut the socket down
                # Stalled or broken connection — close it and reconnect. This is
                # the productionized version of "exit, restart, it works": fresh
                # TCP/TLS state sidesteps whatever stuck the previous one (NAT
                # conntrack drop, congestion-window collapse, server goroutine
                # wedge against just our socket, etc).
                sys.stderr.write(f"\nproducer: send failed ({e}); reconnecting...\n")
                # shutdown() before close(), as the quit path already does: the
                # sender thread may be parked in sendall() on this socket, and
                # closing the fd does not wake a poll() already blocked on it.
                # It would keep waiting on a dead socket while the fresh
                # connection sat idle — and once the fd number is recycled by
                # the reconnect below, write this session's ciphertext into it.
                try: sock.shutdown(socket.SHUT_RDWR)
                except OSError: pass
                # Hold send_lock across the swap so no send is still in flight
                # on the old socket when its fd closes.
                with send_lock:
                    try: sock.close()
                    except OSError: pass
                    sock = connect_with_backoff("reconnect")
                    if sock is None:
                        break  # user pressed q while we were retrying
                    sock_holder[0] = sock
                    sender.take_error()  # drop failures against the old socket
                sys.stderr.write("producer: reconnected\n")
                continue

            if interactive:
                diag.set_phase("mirror")
                if mode == "half":
                    art = ascii_cam.render_half(small)
                else:
                    small_r = cv2.resize(
                        graded, (args.cols, args.rows), interpolation=cv2.INTER_AREA
                    )
                    rampset = ascii_cam.RAMP_CP437 if ramp == "shades" else ascii_cam.RAMP_UNICODE
                    art = ascii_cam.render_ramp(small_r, rampset, True)
                shown = f"{mode}/{ramp}" if mode == "ramp" else mode
                amon = "  a mon:" + ("on" if ctl["amon"] else "off") if args.audio else ""
                status = (f" sat {sat:>3.1f}  con {con:>3.1f}  bri {bri:+d}  mode {shown}"
                          f"   |  +/- sat  [ ] con  < > bri  m mode  g ramp  0 reset{amon}  q quit ")
                out.write("\033[H" + art + "\033[0m")
                if caption:
                    bar = caption.decode("utf-8", "replace")[: args.cols].center(args.cols)
                    out.write(f"\033[{args.rows};1H\033[0;37;44m{bar}\033[0m")
                out.write(f"\033[{args.rows + 1};1H\033[K\033[7m{status[:args.cols]}\033[0m")
                out.flush()

            diag.frame_done()
            n += 1
            if args.frames and n >= args.frames:
                break
            dt = time.monotonic() - t0
            if dt < interval:
                diag.set_phase("idle")
                time.sleep(interval - dt)
    except KeyboardInterrupt:
        pass
    finally:
        if cap is not None:
            cap.release()
        sock.close()
        if interactive and old_term is not None:
            termios.tcsetattr(sys.stdin.fileno(), termios.TCSADRAIN, old_term)
            out.write("\033[?7h\033[0m\033[?25h\n")  # re-enable auto-wrap
            out.flush()


if __name__ == "__main__":
    main()
