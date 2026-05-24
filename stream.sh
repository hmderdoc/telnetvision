#!/usr/bin/env bash
# Stream the webcam to the BBS fanout service using settings from .env.
#
# Extra args are forwarded to producer.py and override the defaults below:
#   ./stream.sh --source test
#   ./stream.sh --cols 120 --rows 50 --fps 20
#
# Required .env keys: BBS_HOST, BBS_PORT, TOKEN
# Optional .env keys: CHANNEL, SOURCE, COLS, ROWS,
#   TLS=0       -> plaintext (service must also run without -tls-cert/-tls-key)
#   INSECURE=1  -> TLS on, but skip cert verification (for a self-signed cert)
#   FLIP=0/1    -> force mirror off/on (default: auto — only --source camera)
#   CAPTION_FILE=path -> broadcast that file's last line as a subtitle
# Set DRYRUN=1 to print the command instead of running it.
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[[ -f .env ]] || { echo "error: .env not found in $(pwd)" >&2; exit 1; }
# Load .env without clobbering vars already in the environment, so an explicit
# `TLS=0 ./stream.sh` overrides the file. (|| [[ -n "$key" ]] catches a final
# line with no trailing newline.)
while IFS='=' read -r key val || [[ -n "$key" ]]; do
  [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue   # skip comments/blanks
  [[ -n "${!key+x}" ]] || export "$key=$val"
done < .env

: "${BBS_HOST:?set BBS_HOST in .env}"
: "${BBS_PORT:?set BBS_PORT in .env}"
: "${TOKEN:?set TOKEN in .env}"

PY="./.venv/bin/python"; [[ -x "$PY" ]] || PY="python3"

args=(
  producer.py
  --host "$BBS_HOST" --port "$BBS_PORT" --token "$TOKEN"
  --channel "${CHANNEL:-cam}" --source "${SOURCE:-camera}"
  --cols "${COLS:-80}" --rows "${ROWS:-24}"
)
# TLS on by default. INSECURE=1 keeps encryption but skips cert verification.
# For full plaintext set TLS=0 (and start the service without -tls-cert/-tls-key).
if [[ "${TLS:-1}" == "1" ]]; then
  args+=(--tls)
  [[ "${INSECURE:-0}" == "1" ]] && args+=(--insecure)
fi
case "${FLIP:-}" in
  0|false|no|off) args+=(--no-flip) ;;
  1|true|yes|on)  args+=(--flip) ;;
esac
[[ -n "${CAPTION_FILE:-}" ]] && args+=(--caption-file "$CAPTION_FILE")
args+=("$@")

if [[ "${DRYRUN:-0}" == "1" ]]; then
  printf '%q ' "$PY" "${args[@]}"; echo
  exit 0
fi
exec "$PY" "${args[@]}"
