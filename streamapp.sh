#!/usr/bin/env bash
# Launch a macOS app and stream the display it's on to the BBS, via ffmpeg.
#
#   ./streamapp.sh "App Name" [screen#] [WxH]
#       screen#  which display (0,1,2…), default 0
#       WxH      frame size piped to the producer, default 640x480
#
# IMPORTANT: macOS/ffmpeg capture a whole DISPLAY, not a single window. The app
# is launched and brought to front; you stream that display. For a single window
# use OBS "Window Capture" -> Virtual Camera (then `SOURCE=<index> ./stream.sh`).
#
# Requires: ffmpeg (brew install ffmpeg) and Screen Recording permission for your
# terminal: System Settings > Privacy & Security > Screen Recording.
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

app="${1:?usage: ./streamapp.sh \"App Name\" [screen#] [WxH]}"
screen_no="${2:-0}"
size="${3:-640x480}"
fps=15   # keep equal to the producer's --fps so the pipe doesn't back up

command -v ffmpeg >/dev/null || { echo "ffmpeg not found (brew install ffmpeg)" >&2; exit 1; }

# Resolve "Capture screen <screen_no>" to its avfoundation device index.
# (ffmpeg -list_devices always exits non-zero; `|| true` keeps set -e happy.)
devices="$(ffmpeg -f avfoundation -list_devices true -i "" 2>&1 || true)"
idx="$(printf '%s\n' "$devices" \
  | sed -nE "s/.*\] \[([0-9]+)\] Capture screen ${screen_no}\$/\1/p" | head -1 || true)"
if [[ -z "$idx" ]]; then
  echo "Could not find 'Capture screen ${screen_no}'. Available screens:" >&2
  printf '%s\n' "$devices" | grep -i 'capture screen' >&2 || echo "  (none — grant your terminal Screen Recording permission)" >&2
  exit 1
fi
echo "screen '${screen_no}' = avfoundation device ${idx}; size ${size}; launching ${app}…" >&2

# Try as an app name, then as a URL/file (e.g. https://tv.youtube.com).
open -a "$app" 2>/dev/null || open "$app" 2>/dev/null \
  || echo "warning: couldn't launch '${app}' — streaming the current screen anyway" >&2
sleep 2

# Capture -> raw bgr24 -> the producer's stdin source. --in-size MUST match the
# scale below. Quit with q in… well, there's no mirror over a pipe: Ctrl-C here.
# -pixel_format bgr0: the screen device rejects ffmpeg's default (yuv420p).
# Do NOT set input -framerate: avfoundation screen capture rejects it and falls
# back to a bogus 1000k-fps timebase, which makes ffmpeg spew duplicate frames.
# Instead normalize the OUTPUT rate with the fps filter.
ffmpeg -hide_banner -loglevel warning -stats \
  -f avfoundation -capture_cursor 1 -pixel_format bgr0 -i "${idx}:none" \
  -vf "fps=${fps},scale=${size/x/:},format=bgr24" -fps_mode cfr -f rawvideo - \
| ./stream.sh --source - --in-size "$size" --fps "$fps"
