# Telnetvision

Real-time webcam / video → ASCII, streamed to a [Synchronet](https://www.synchro.net/) BBS (or any terminal). Capture on your machine, push out to a tiny relay in the cloud, and BBS callers watch a live half-block / ASCII rendering — in glorious CP437 or 24-bit color — right in their terminal.

```
   HOME (your machine)              CLOUD                          BBS caller
 ┌────────────────────┐        ┌──────────────┐               ┌──────────────┐
 │ producer.py        │        │ service      │   localhost   │ door         │
 │  capture webcam /  │──TLS──▶│  fanout relay│──────────────▶│  renders     │──▶ SyncTERM
 │  HDMI / file / pipe│  push  │  (1 producer │   per caller  │  CP437/ANSI  │
 │  grade + downscale │  out   │   → N callers│               │  half-blocks │
 └────────────────────┘        └──────────────┘               └──────────────┘
        (Python)                    (Go)                            (Go)
```

- **The home side dials out** — your network needs no inbound ports.
- **One producer fans out to many callers**; each caller's door paces to its own link.
- **Live captions**: pipe a speech-to-text feed (e.g. whisper.cpp on your mic) and the door draws a subtitle bar.

## Pieces

| Component | Runs on | What it does |
|-----------|---------|--------------|
| `producer.py` | your machine (Python) | captures a source, color-grades, downscales, pushes frames + render directives + captions |
| `service` | cloud box (Go) | channel-keyed fanout relay; one publisher per channel, many subscribers, drop-to-latest |
| `door` | BBS box (Go) | per-caller renderer → CP437/UTF-8 half-blocks or ramp, truecolor or 16-color, delta-encoded, latency-paced |
| `ascii_cam.py` | your machine | standalone local viewer (no streaming) |

## Quickstart — home side

```bash
python3 -m venv .venv && ./.venv/bin/pip install -r requirements.txt
cp .env.example .env          # set BBS_HOST, TOKEN, etc.
./stream.sh                   # camera → your BBS (live mirror + key controls)
```

In the mirror: `+/-` saturation · `[ ]` contrast · `< >` brightness · `m` half-block↔ramp · `g` ramp glyphs · `q` quit.

Sources (set `SOURCE=` in `.env` or `--source`): `camera` · a device index (e.g. an HDMI capture card or OBS virtual cam) · a video file / URL · `-` (raw stdin from ffmpeg) · `test`.

**One command with live mic captions:** `./live.sh` (see [caption-mic.sh](caption-mic.sh)).

## Quickstart — BBS / cloud side

Grab the release bundle for your OS/arch (or build — see below), then follow **[packaging/INSTALL.md](packaging/INSTALL.md)**. In short: run `service` as a daemon (it listens for the producer on `:7600` and for doors on `127.0.0.1:7601`), and add `door` as a Synchronet external program. Door behavior is tuned live via [packaging/door.ini](packaging/door.ini) — no BBS restart needed.

## Build from source

```bash
(cd service && go build -o ../bin/service .)
(cd door    && go build -o ../bin/door .)
```
Go cross-compiles to Linux/macOS/Windows × amd64/arm64; the release workflow builds all of them. The `door` keeps its Unix latency-pacer behind build tags and falls back to blocking I/O on Windows.

## Captions (optional)

`caption-mic.sh` runs [whisper.cpp](https://github.com/ggerganov/whisper.cpp) on a chosen audio input and writes the current line to `CAPTION_FILE`; the producer broadcasts it and the door draws a subtitle bar. Get a model with `models/download.sh`.

## License

[MIT](LICENSE).
