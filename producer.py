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
  0    reset grading      q  quit
Dimensions are fixed per session (set --cols/--rows); they are not live-editable
because changing them resizes every BBS caller's screen.
"""
import argparse
import os
import select
import socket
import ssl
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
    ap.add_argument("--mirror", action=argparse.BooleanOptionalAction, default=True,
                    help="live local preview + key controls (auto-off if not a TTY)")
    ap.add_argument("--frames", type=int, default=0, help="stop after N frames (0=forever)")
    ap.add_argument("--diag-file", default="",
                    help="write per-phase timings here every 2s; a watchdog logs "
                         "'STUCK in phase X for Ys' if any phase blocks > 3s")
    ap.add_argument("--send-timeout", type=float, default=10.0,
                    help="seconds before a wedged sendall() fails with TimeoutError "
                         "instead of blocking forever (default: 10)")
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

    sock = connect(args)
    # Bound any single socket operation. A wedged service used to leave
    # sendall() blocked forever; with a timeout it raises TimeoutError so the
    # producer can report and exit instead of looking alive but stuck.
    sock.settimeout(args.send_timeout)
    wire.hello_producer(sock, args.token, args.channel)

    # Live-adjustable state, shared with the stdin reader thread. Python's GIL
    # makes single-key writes/reads atomic; we snapshot at the top of each frame.
    ctl = {"sat": args.saturation, "con": args.contrast, "bri": args.brightness,
           "mode": args.mode, "ramp": args.ramp, "quit": False}

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
                    elif k in ("q", "Q"):
                        ctl["quit"] = True
                        try:
                            sock.shutdown(socket.SHUT_RDWR)
                        except OSError:
                            pass
                        return

        threading.Thread(target=stdin_reader, daemon=True).start()

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
            try:
                wire.send_msg(sock, wire.frame_payload(
                    args.cols, args.rows, pixels.tobytes(), mode_i, ramp_i, caption))
            except TimeoutError:
                sys.stderr.write(
                    f"\nproducer: send to {args.host}:{args.port} timed out after "
                    f"{args.send_timeout:.0f}s — service stopped reading.\n"
                    f"  on the cloud box: check the service is alive and not deadlocked\n"
                    f"  (`kill -USR1 <service-pid>` dumps every goroutine's stack to its log)\n")
                sys.exit(2)
            except OSError as e:
                if ctl["quit"]:
                    break  # user pressed q; the stdin thread shut the socket down
                sys.stderr.write(f"\nproducer: send failed: {e}\n")
                sys.exit(2)

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
                status = (f" sat {sat:>3.1f}  con {con:>3.1f}  bri {bri:+d}  mode {shown}"
                          f"   |  +/- sat  [ ] con  < > bri  m mode  g ramp  0 reset  q quit ")
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
