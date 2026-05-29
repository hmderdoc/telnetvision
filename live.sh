#!/usr/bin/env bash
# Stream + live mic captions in ONE command. Mic captioning runs in the
# background; the video stream (mirror + keys) runs in the foreground. Quit the
# stream with `q` and both stop (and the caption clears). Extra args pass through
# to stream.sh.
#
#   ./live.sh                 # camera + mic captions
#   ./live.sh --source 1      # HDMI capture card + mic captions
#   MIC_MODEL=small.en ./live.sh   # bigger caption model
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Strip a matching pair of surrounding quotes from a .env value
# (so CAPTURE="usb audio" works the same as CAPTURE=usb\ audio).
unquote() {
  local v="$1"
  # strip a matching pair of surrounding quotes; #?/%? works on bash 3.2 (macOS).
  if [[ "$v" == \"*\" || "$v" == \'*\' ]]; then v="${v#?}"; v="${v%?}"; fi
  printf '%s' "$v"
}

# Use the same caption file the producer reads (from .env), so they line up.
cf="$(unquote "$(grep -E '^CAPTION_FILE=' .env 2>/dev/null | tail -1 | cut -d= -f2-)")"
export CAPTION_FILE="${cf:-/tmp/caption.txt}"

# CAPTURE = which audio input whisper-stream transcribes (see caption-mic.sh).
# An explicit `CAPTURE=2 ./live.sh` wins over .env.
if [[ -z "${CAPTURE:-}" ]]; then
  c="$(unquote "$(grep -E '^CAPTURE=' .env 2>/dev/null | tail -1 | cut -d= -f2-)")"
  [[ -n "$c" ]] && export CAPTURE="$c"
fi

./caption-mic.sh "${MIC_MODEL:-base.en}" >/tmp/caption-mic.log 2>&1 &
CAP_PID=$!
cleanup() { pkill -P "$CAP_PID" 2>/dev/null; kill "$CAP_PID" 2>/dev/null; }
trap cleanup EXIT INT TERM

./stream.sh "$@"
