#!/usr/bin/env python3
"""Real-time webcam -> ASCII, rendered in the terminal."""
import argparse
import shutil
import signal
import sys
import time

import cv2
import numpy as np

# Brightness ramps, dark -> light.
RAMP_UNICODE = np.array(list(" .:-=+*#%@"))     # ASCII art ramp (default)
RAMP_CP437 = np.array(list(" ░▒▓█"))  # CP437 shades:  ░ ▒ ▓ █

UPPER_HALF = "▀"  # ▀  top half = fg color, bottom half = bg color

HIDE_CURSOR = "\033[?25l"
SHOW_CURSOR = "\033[?25h"
CLEAR = "\033[2J"
HOME = "\033[H"
RESET = "\033[0m"


def grid_size(frame_w, frame_h, cell_aspect=0.5):
    """Pick a char grid that fits the terminal and preserves proportions.

    cell_aspect ~ glyph width / height; terminal cells are ~2x taller than
    wide, so we squash vertical resolution to keep the image from stretching.
    """
    term = shutil.get_terminal_size((80, 24))
    cols = term.columns
    rows = max(1, int(cols * (frame_h / frame_w) * cell_aspect))
    # Leave one row free so the shell prompt doesn't scroll the frame.
    rows = min(rows, term.lines - 1)
    return cols, rows


def render_ramp(small, ramp, color):
    """Brightness -> character. `small` is (rows, cols, 3) BGR."""
    gray = cv2.cvtColor(small, cv2.COLOR_BGR2GRAY)
    idx = (gray.astype(np.int32) * (len(ramp) - 1)) // 255
    chars = ramp[idx]
    if not color:
        return "\n".join("".join(row) for row in chars)
    b, g, r = small[:, :, 0], small[:, :, 1], small[:, :, 2]
    lines = []
    for y in range(chars.shape[0]):
        parts = [
            f"\033[38;2;{r[y, x]};{g[y, x]};{b[y, x]}m{chars[y, x]}"
            for x in range(chars.shape[1])
        ]
        lines.append("".join(parts))
    return "\n".join(lines) + RESET


def render_half(small):
    """Half-block pixel art. `small` is (2*rows, cols, 3) BGR.

    Each cell stacks two source pixels: the top is the glyph's foreground,
    the bottom its background. Doubles vertical resolution -> square pixels.
    """
    rows = small.shape[0] // 2
    top = small[0 : 2 * rows : 2]   # even scanlines -> upper pixel
    bot = small[1 : 2 * rows : 2]   # odd scanlines  -> lower pixel
    lines = []
    for y in range(rows):
        parts = []
        for x in range(small.shape[1]):
            tb, tg, tr = top[y, x]
            bb, bg, br = bot[y, x]
            parts.append(
                f"\033[38;2;{tr};{tg};{tb};48;2;{br};{bg};{bb}m{UPPER_HALF}"
            )
        lines.append("".join(parts) + RESET)  # reset so bg doesn't bleed
    return "\n".join(lines)


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--camera", type=int, default=0, help="capture device index")
    ap.add_argument("--fps", type=float, default=20.0, help="target frames/sec")
    ap.add_argument(
        "--mode",
        choices=["ramp", "half"],
        default="ramp",
        help="ramp: brightness->char; half: half-block pixel art (always color)",
    )
    ap.add_argument(
        "--charset",
        choices=["unicode", "cp437"],
        default="unicode",
        help="ramp glyphs: unicode ASCII ramp, or cp437 shades ' ░▒▓█'",
    )
    ap.add_argument("--color", action="store_true", help="24-bit color (ramp mode)")
    args = ap.parse_args()

    cap = cv2.VideoCapture(args.camera)
    if not cap.isOpened():
        sys.exit(f"Could not open camera {args.camera}")

    out = sys.stdout
    out.write(HIDE_CURSOR + CLEAR)
    out.flush()

    def cleanup(*_):
        cap.release()
        out.write(SHOW_CURSOR + RESET + "\n")
        out.flush()
        sys.exit(0)

    signal.signal(signal.SIGINT, cleanup)

    frame_interval = 1.0 / args.fps
    try:
        while True:
            t0 = time.monotonic()
            ok, frame = cap.read()
            if not ok:
                continue
            frame = cv2.flip(frame, 1)  # mirror, like a selfie cam
            h, w = frame.shape[:2]
            cols, rows = grid_size(w, h)
            if args.mode == "half":
                small = cv2.resize(
                    frame, (cols, 2 * rows), interpolation=cv2.INTER_AREA
                )
                art = render_half(small)
            else:
                small = cv2.resize(frame, (cols, rows), interpolation=cv2.INTER_AREA)
                ramp = RAMP_CP437 if args.charset == "cp437" else RAMP_UNICODE
                art = render_ramp(small, ramp, args.color)
            out.write(HOME + art)
            out.flush()

            dt = time.monotonic() - t0
            if dt < frame_interval:
                time.sleep(frame_interval - dt)
    finally:
        cleanup()


if __name__ == "__main__":
    main()
