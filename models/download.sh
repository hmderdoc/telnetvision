#!/usr/bin/env bash
# Download a whisper.cpp GGML model for live captions (used by caption-mic.sh).
#   ./models/download.sh            # base.en  (~142 MB, default)
#   ./models/download.sh small.en   # better accuracy (~466 MB)
#   ./models/download.sh tiny.en    # fastest / lowest latency (~75 MB)
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

m="${1:-base.en}"
url="https://huggingface.co/ggerganov/whisper.cpp/resolve/main/ggml-${m}.bin"
echo "downloading ggml-${m}.bin from huggingface ..."
curl -L --fail -o "ggml-${m}.bin" "$url"
echo "saved models/ggml-${m}.bin"
