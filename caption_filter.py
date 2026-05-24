#!/usr/bin/env python3
"""Turn whisper-stream's live output into a single 'current caption' file.

whisper-stream updates the active line in place with \\r (sliding-window mode)
and commits with \\n. We treat both as "the line changed" and overwrite the
caption file with the latest *speech* text — stripping ANSI codes and the
[timestamp] prefix, and dropping non-speech annotations like [Music],
[BLANK_AUDIO], (applause), or bare ♪ that whisper emits on music/silence.
The producer reads that file's last line and broadcasts it.
"""
import re
import sys

if len(sys.argv) < 2:
    sys.exit("usage: caption_filter.py <caption-file>")
out = sys.argv[1]

ansi = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]")
tstamp = re.compile(r"^\s*\[[\d:.\s>\-]+\]\s*")   # leading [00:00.000 --> 00:02.000]
annot = re.compile(r"^[\[(<].*[\])>]$")            # a whole annotation: [Music], (applause)
nonspeech = re.compile(r"^[\s♪♫*_.~\-–—]*$")       # only music notes / filler / blank


def write(raw: bytes) -> None:
    s = ansi.sub("", raw.decode("utf-8", "replace"))
    s = tstamp.sub("", s).strip()
    if not s or annot.match(s) or nonspeech.match(s):
        return  # empty, [Music], (sound effects), ♪♪♪, [BLANK_AUDIO], etc.
    try:
        with open(out, "wb") as f:
            f.write(s.encode("utf-8") + b"\n")
    except OSError:
        pass


buf = bytearray()
stdin = sys.stdin.buffer
while True:
    ch = stdin.read(1)
    if not ch:
        write(bytes(buf))
        break
    if ch in (b"\n", b"\r"):
        write(bytes(buf))
        buf.clear()
    else:
        buf += ch
