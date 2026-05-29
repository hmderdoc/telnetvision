#!/usr/bin/env bash
# Live mic captions: whisper.cpp transcribes a chosen audio input in real time
# and writes the current line to the caption file the producer broadcasts. Run
# alongside ./stream.sh (which broadcasts $CAPTION_FILE).
#
#   ./caption-mic.sh                        # base.en model, default input
#   ./caption-mic.sh small.en               # use models/ggml-small.en.bin
#   CAPTURE=2 ./caption-mic.sh              # SDL device index (fast; brittle if devices shuffle)
#   CAPTURE="usb audio" ./caption-mic.sh    # case-insensitive name substring
#                                           # (resilient when devices reorder, e.g. AirPods
#                                           #  toggling; pays ~10s discovery on startup)
#   LIST=1 ./caption-mic.sh                 # just print the current device list and exit
#   RESOLVE_ONLY=1 CAPTURE=... ./caption-mic.sh   # resolve a name to an index and exit
#
# First run prompts your terminal for Microphone permission. Ctrl-C to stop
# (clears the caption so the subtitle bar disappears).
set -uo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

MODEL="models/ggml-${1:-base.en}.bin"
OUT="${CAPTION_FILE:-/tmp/caption.txt}"
CAP="${CAPTURE:--1}"
WLOG="${WLOG:-/tmp/whisper.log}"

command -v whisper-stream >/dev/null || { echo "whisper-stream not found (brew install whisper-cpp)" >&2; exit 1; }
[[ -f "$MODEL" ]] || { echo "model not found: $MODEL  (see models/ download)" >&2; exit 1; }

# Briefly run whisper-stream just long enough to print its SDL capture-device
# list into $WLOG, then kill it. The default device gets opened (and a brief
# bit of its audio captured + discarded) along the way. ~10s on Metal.
discover_devices() {
  : > "$WLOG"
  whisper-stream -m "$MODEL" -c -1 -t 1 --step 999999 --length 999999 --keep 0 \
    >/dev/null 2>"$WLOG" &
  local pid=$! i
  disown 2>/dev/null || true   # stop bash from printing "Abort trap" on kill
  for i in $(seq 1 80); do
    sleep 0.5
    grep -q 'attempt to open default' "$WLOG" 2>/dev/null && break
    kill -0 "$pid" 2>/dev/null || break
  done
  kill "$pid" 2>/dev/null || true
}

list_devices() { grep -E "Capture device #[0-9]+:" "$WLOG" 2>/dev/null; }

# Resolve $1 to an SDL device index: integer pass-through; anything else looks
# for an *exact* (case-insensitive) name match first, then falls back to a
# substring match. If a substring matches more than one device, lists the
# candidates and errors so you can pick a more specific name.
resolve_capture() {
  local pat="$1"
  if [[ "$pat" =~ ^-?[0-9]+$ ]]; then
    echo "$pat"; return 0
  fi
  echo "resolving audio device matching '$pat' (~10s discovery)..." >&2
  discover_devices
  local match
  # 1) exact case-insensitive name match
  match="$(list_devices | awk -F"'" -v p="$pat" 'BEGIN{p=tolower(p)} tolower($2)==p {print; exit}')"
  if [[ -z "$match" ]]; then
    # 2) substring match — must be unique
    local candidates count
    candidates="$(list_devices | grep -i -- "$pat")"
    if [[ -z "$candidates" ]]; then
      echo "no audio device matching '$pat'. Current devices:" >&2
      { list_devices || echo "(no devices discovered)"; } >&2
      return 1
    fi
    count="$(printf '%s\n' "$candidates" | wc -l | tr -d ' ')"
    if [[ "$count" -gt 1 ]]; then
      echo "'$pat' matches multiple devices; use a more specific name:" >&2
      printf '%s\n' "$candidates" >&2
      return 1
    fi
    match="$candidates"
  fi
  local idx name
  idx="$(printf '%s\n' "$match" | sed -nE "s/.*Capture device #([0-9]+):.*/\1/p")"
  name="$(printf '%s\n' "$match" | sed -nE "s/.*Capture device #[0-9]+: *'([^']*)'.*/\1/p")"
  echo "matched device #$idx: $name" >&2
  echo "$idx"
}

if [[ "${LIST:-0}" == "1" ]]; then
  echo "discovering audio devices (~10s)..." >&2
  discover_devices
  list_devices
  exit 0
fi

CAP_ARG="$(resolve_capture "$CAP")" || exit 1

if [[ "${RESOLVE_ONLY:-0}" == "1" ]]; then
  echo "$CAP_ARG"
  exit 0
fi

trap ': > "$OUT" 2>/dev/null || true' EXIT   # clear the caption on stop
: > "$OUT"
echo "captioning mic -> $OUT  (model: $MODEL, device: $CAP_ARG; Ctrl-C to stop)" >&2
echo "whisper log + capture-device list: $WLOG" >&2

# Sliding window: re-transcribe the last 5s every 700ms; the filter keeps only
# the latest speech line in $OUT. whisper's stderr (incl. its device list) -> WLOG.
# If you get [Music]/[sound effects], you're on the wrong input — try
# CAPTURE="<name substring>" so it survives device reorderings.
whisper-stream -m "$MODEL" -c "$CAP_ARG" -t 6 --step 700 --length 5000 --keep 200 2>"$WLOG" \
  | python3 caption_filter.py "$OUT"
