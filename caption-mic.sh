#!/usr/bin/env bash
# Live mic captions: whisper.cpp transcribes your microphone in real time and
# writes the current line to the caption file the producer broadcasts. Run this
# alongside ./stream.sh (which broadcasts $CAPTION_FILE).
#
#   ./caption-mic.sh                # base.en model, /tmp/caption.txt (or $CAPTION_FILE)
#   ./caption-mic.sh small.en       # use models/ggml-small.en.bin instead
#   CAPTURE=2 ./caption-mic.sh      # pick a specific input device (whisper-stream -c ID)
#
# First run prompts your terminal for Microphone permission. Ctrl-C to stop
# (clears the caption so the subtitle bar disappears).
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

MODEL="models/ggml-${1:-base.en}.bin"
OUT="${CAPTION_FILE:-/tmp/caption.txt}"
CAP="${CAPTURE:--1}"   # -1 = system default input

command -v whisper-stream >/dev/null || { echo "whisper-stream not found (brew install whisper-cpp)" >&2; exit 1; }
[[ -f "$MODEL" ]] || { echo "model not found: $MODEL  (see models/ download)" >&2; exit 1; }

WLOG="${WLOG:-/tmp/whisper.log}"
trap ': > "$OUT" 2>/dev/null || true' EXIT   # clear the caption on stop
: > "$OUT"
echo "captioning mic -> $OUT  (model: $MODEL, device: $CAP; Ctrl-C to stop)" >&2
echo "whisper log + capture-device list: $WLOG" >&2

# Sliding window: re-transcribe the last 5s every 700ms; the filter keeps only
# the latest speech line in $OUT. whisper's stderr (incl. its device list) -> WLOG.
# If you get [Music]/[sound effects], you're on the wrong input — check WLOG for the
# device list, then CAPTURE=<id> ./caption-mic.sh (or set the mic as macOS default input).
whisper-stream -m "$MODEL" -c "$CAP" -t 6 --step 700 --length 5000 --keep 200 2>"$WLOG" \
  | python3 caption_filter.py "$OUT"
