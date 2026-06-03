#!/usr/bin/env python3
"""Caption the audio of any URL/file ffmpeg can read (HDHomeRun MPEG-TS,
RTSP, mp4, ...) by demuxing the audio track and feeding fixed-size chunks to
whisper-cli. The cleaned transcription is written to CAPTION_FILE, which the
producer is already broadcasting — no producer changes needed.

This complements caption-mic.sh: that one transcribes a local mic;
this one transcribes the source's audio (when the source has audio).

  python caption-source.py http://192.168.0.23:5005/auto/v2.1
  SOURCE=clip.mp4 CAPTION_MODEL=small.en python caption-source.py
  CHUNK_SECS=8 python caption-source.py rtsp://cam.local/stream

Env (all optional):
  SOURCE         URL/file/path (falls back to argv[1])
  CAPTION_FILE   <tempdir>/caption.txt  (/tmp on Linux/macOS, %TEMP% on Windows)
  CAPTION_MODEL  base.en   (resolved as models/ggml-<name>.bin if not a path)
  CHUNK_SECS     5         (lower = less latency, lower accuracy)
  WHISPER_BIN    whisper-cli
  FFMPEG         ffmpeg
  THREADS        4

Latency budget: roughly CHUNK_SECS seconds of audio + the time whisper-cli
takes to transcribe one chunk (~1-3s on CPU with base.en). For BBS captions
that's fine; broadcast TV captions run 2-7s behind too.

Note: the producer is also consuming SOURCE for video. Most HTTP/RTSP sources
allow concurrent consumers; if yours doesn't (some HDHomeRun endpoints don't),
point one of them at a local tee or a recording instead."""
import os
import signal
import subprocess
import sys
import tempfile
import wave
from pathlib import Path

import caption_filter

SAMPLE_RATE = 16000
BYTES_PER_SAMPLE = 2  # s16le mono


def resolve_model(name: str) -> str:
    """Accept either a path to a .bin or a short name like 'base.en' that
    expands to models/ggml-<name>.bin next to this script."""
    if os.path.isfile(name):
        return name
    here = Path(__file__).resolve().parent
    p = here / "models" / f"ggml-{name}.bin"
    if p.is_file():
        return str(p)
    sys.exit(f"caption-source: model not found: {name} (looked at: {p})\n"
             f"  download one: ./models/download.sh base.en")


def transcribe(wav_path: str, model: str, whisper_bin: str, threads: int) -> str:
    """Run whisper-cli once on wav_path; return the cleaned single-line result."""
    cmd = [whisper_bin, "-m", model, "-f", wav_path,
           "-nt", "-np", "-sns", "-l", "en", "-t", str(threads)]
    try:
        # whisper-cli writes UTF-8; pin it so Windows doesn't fall back to cp1252.
        r = subprocess.run(cmd, capture_output=True, timeout=60,
                           encoding="utf-8", errors="replace")
    except subprocess.TimeoutExpired:
        return ""
    if r.returncode != 0:
        sys.stderr.write(r.stderr[-200:] + "\n")
        return ""
    # whisper-cli emits one transcript line per detected segment; collapse to one
    parts = [caption_filter.clean(line) for line in r.stdout.splitlines()]
    return " ".join(p for p in parts if p)


def write_caption(path: str, text: str) -> None:
    """Overwrite the caption file with the latest line (matches caption-mic.sh
    semantics — the producer always reads the LAST line)."""
    try:
        with open(path, "wb") as f:
            f.write(text.encode("utf-8") + b"\n")
    except OSError as e:
        sys.stderr.write(f"caption-source: write {path}: {e}\n")


def main() -> int:
    source = os.environ.get("SOURCE") or (sys.argv[1] if len(sys.argv) > 1 else "")
    if not source:
        sys.exit("caption-source: SOURCE is required (env or argv[1]).\n"
                 "  example: caption-source.py http://192.168.0.23:5005/auto/v2.1")
    # Cross-platform default: /tmp on Unix, %TEMP% on Windows. Most users will
    # set CAPTION_FILE explicitly to keep producer + caption-source in sync.
    default_caption = str(Path(tempfile.gettempdir()) / "caption.txt")
    caption_file = os.environ.get("CAPTION_FILE", default_caption)
    model = resolve_model(os.environ.get("CAPTION_MODEL", "base.en"))
    chunk_secs = float(os.environ.get("CHUNK_SECS", "5"))
    whisper_bin = os.environ.get("WHISPER_BIN", "whisper-cli")
    ffmpeg = os.environ.get("FFMPEG", "ffmpeg")
    threads = int(os.environ.get("THREADS", "4"))
    chunk_bytes = int(chunk_secs * SAMPLE_RATE * BYTES_PER_SAMPLE)

    sys.stderr.write(
        f"caption-source: source={source!r} chunk={chunk_secs}s "
        f"model={Path(model).name} -> {caption_file}\n")

    # ffmpeg: demux source, drop video, mono 16k s16le PCM to stdout.
    ff_cmd = [ffmpeg, "-nostdin", "-loglevel", "warning", "-i", source,
              "-vn", "-map", "0:a:0?", "-ac", "1", "-ar", str(SAMPLE_RATE),
              "-f", "s16le", "-"]
    ff = subprocess.Popen(ff_cmd, stdout=subprocess.PIPE, stderr=sys.stderr)

    # Clean up ffmpeg on Ctrl-C / SIGTERM so the source connection is released.
    def stop(*_):
        try:
            ff.terminate()
        except Exception:
            pass
        sys.exit(0)
    signal.signal(signal.SIGINT, stop)
    # SIGTERM exists on Windows Python but is never raised by the OS — and on
    # some Python/Windows combos signal.signal(SIGTERM, ...) raises ValueError.
    try:
        signal.signal(signal.SIGTERM, stop)
    except (ValueError, AttributeError):
        pass

    def flush(chunk: bytes) -> None:
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tf:
            wav_path = tf.name
        try:
            with wave.open(wav_path, "wb") as w:
                w.setnchannels(1)
                w.setsampwidth(BYTES_PER_SAMPLE)
                w.setframerate(SAMPLE_RATE)
                w.writeframes(chunk)
            text = transcribe(wav_path, model, whisper_bin, threads)
            if text:
                write_caption(caption_file, text)
        finally:
            try:
                os.unlink(wav_path)
            except OSError:
                pass

    buf = bytearray()
    min_tail = SAMPLE_RATE * BYTES_PER_SAMPLE // 2  # 0.5s — anything shorter is noise
    try:
        while True:
            data = ff.stdout.read(8192)
            if not data:
                if ff.poll() is not None:
                    break
                continue
            buf.extend(data)
            while len(buf) >= chunk_bytes:
                chunk, buf = bytes(buf[:chunk_bytes]), bytearray(buf[chunk_bytes:])
                flush(chunk)
        # Trailing audio shorter than CHUNK_SECS (the whole input, for short
        # files; the final partial window, for any file): transcribe it too.
        if len(buf) >= min_tail:
            flush(bytes(buf))
    finally:
        try:
            ff.terminate()
            ff.wait(timeout=2)
        except subprocess.TimeoutExpired:
            ff.kill()
        except Exception:
            pass
    return ff.returncode or 0


if __name__ == "__main__":
    sys.exit(main())
