#!/usr/bin/env python3
"""Turn a live transcription stream into a single 'current caption' file.

Used two ways:

  * `whisper-stream | caption_filter.py <out>` — strips ANSI codes and the
    sliding-window [timestamp] prefix, breaks on \\r or \\n (whisper-stream
    overwrites the current line in place), drops non-speech annotations like
    [Music], [BLANK_AUDIO], (applause), or bare ♪♪♪.
  * `from caption_filter import clean` — the same regex pipeline as a function
    for use by source-audio captioning (whisper-cli per-chunk output).
"""
import re
import sys

ansi = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")
tstamp = re.compile(r"^\s*\[[\d:.\s>\-]+\]\s*")  # leading [00:00.000 --> 00:02.000]
annot = re.compile(r"^[\[(<].*[\])>]$")           # whole-line annotation: [Music], (applause)
nonspeech = re.compile(r"^[\s♪♫*_.~\-–—]*$")      # only music notes / filler / blank


def clean(s: str) -> str:
    """Return s with noise stripped, or '' if the whole line should be dropped."""
    s = ansi.sub("", s)
    s = tstamp.sub("", s).strip()
    if not s or annot.match(s) or nonspeech.match(s):
        return ""
    return s


def overwrite(path: str, raw: bytes) -> None:
    """Decode `raw` and, if anything useful survives `clean()`, overwrite `path`
    with that single line. Used by the whisper-stream pipeline where each line
    is the *current* caption (replacing the previous one)."""
    s = clean(raw.decode("utf-8", "replace"))
    if not s:
        return
    try:
        with open(path, "wb") as f:
            f.write(s.encode("utf-8") + b"\n")
    except OSError:
        pass


def _cli():
    if len(sys.argv) < 2:
        sys.exit("usage: caption_filter.py <caption-file>")
    out = sys.argv[1]
    buf = bytearray()
    stdin = sys.stdin.buffer
    while True:
        ch = stdin.read(1)
        if not ch:
            overwrite(out, bytes(buf))
            break
        if ch in (b"\n", b"\r"):
            overwrite(out, bytes(buf))
            buf.clear()
        else:
            buf += ch


if __name__ == "__main__":
    _cli()
