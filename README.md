# Telnetvision

**Real-time webcam / video → ASCII, streamed to a [Synchronet](https://www.synchro.net/) BBS (or any terminal).**

Capture on your machine, push out to a tiny relay in the cloud, and BBS callers watch a live half-block / ASCII rendering — CP437 or 24-bit color — right in their terminal. Add live subtitles from your mic. It's a webcam channel for the BBS era.

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

## Features

- **Sources**: webcam, HDMI capture card, OBS virtual camera, video files, RTSP/HTTP streams, or a raw `ffmpeg` pipe.
- **Renders**: half-block "pixel art" (`▀`) or brightness ramp; **24-bit truecolor** or - **Live color grading** from the producer (saturation / contrast / brightness), applied before send so every caller sees it.
- **Live captions**: feed any speech-to-text into a file and the door draws a subtitle bar; includes a whisper.cpp mic setup.
- **One producer → many callers**, each paced independently. **Latency-bounded**: the door drops stale frames instead of letting a slow link build a backlog (delta-encoded, non-blocking output).
- **Dials out** from home — no inbound ports on your network. Token-authenticated, TLS-capable ingest.
- **Tune the BBS view live** via `door.ini` — no Synchronet restart.

## How it works

The producer captures a frame, color-grades it, downscales to the terminal cell grid, and pushes a compact message (RGB pixels + a render directive + an optional caption) to the **service**. The service is a domain-agnostic fanout relay: one publisher per channel, many subscribers, keeping only the latest frame per subscriber (drop-to-latest). Each **door** — launched by Synchronet per caller — turns those pixels into CP437/ANSI half-blocks or a ramp, in the caller's color depth, **delta-encoding** only the cells that changed, and writes via a **non-blocking pacer** that never queues more than it can flush (so latency stays bounded on slow links). The wire format is documented in [`wire.py`](wire.py).

## Install

### Home (capture) side

```bash
python3 -m venv .venv && ./.venv/bin/pip install -r requirements.txt
cp .env.example .env          # set BBS_HOST, BBS_PORT, TOKEN, ...
./stream.sh                   # camera → your BBS, with a live local mirror
```

Mirror keys: `+/-` saturation · `[ ]` contrast · `< >` brightness · `m` half-block↔ramp · `g` ramp glyphs · `q` quit.

### BBS / cloud side

Download the release bundle for your OS/arch (or build — see below) and follow **[packaging/INSTALL.md](packaging/INSTALL.md)**. In short: run `service` as a daemon (listens for the producer on `:7600`, for doors on `127.0.0.1:7601`), open only `:7600` inbound, and add `door` as a Synchronet external program (Intercept Standard I/O = Yes, Multiple Concurrent Users = Yes).

## Configuration

### The producer `.env`

`stream.sh` (and `live.sh`) read settings from a `.env` file in the project root — copy `.env.example` to `.env` and edit. Every key can also be given as a one-off override on the command line (`COLS=120 ./stream.sh`) or as a producer flag (`./stream.sh --cols 120`); an explicit override wins over `.env`. A typical `.env`:

```ini
# Where the relay lives, and the shared secret to publish to it.
BBS_HOST=your.bbs.hostname     # the cloud box running `service`
BBS_PORT=7600                  # its ingest port (matches service -ingest)
TOKEN=cfc29cba...              # matches service -token; make one: openssl rand -hex 16

# How big a picture to send (in terminal cells; a BBS screen is 80x24/25).
COLS=80
ROWS=24

# Transport security.
TLS=1                          # 1 = encrypt (service needs a cert); 0 = plaintext
INSECURE=1                     # 1 = don't verify the cert (self-signed); 0 = verify (real cert)

# Optional extras (safe to omit):
CAPTION_FILE=/tmp/caption.txt  # broadcast this file's last line as a subtitle
# SOURCE=camera                # what to capture (see below)
# CAPTURE=2                    # which audio input to transcribe (see Live captions)
# FLIP=0                       # mirror off (auto-mirrors only the default webcam)
```

| Key | Example | What it does |
|-----|---------|--------------|
| `BBS_HOST` | `futureland.today` | Hostname/IP of the cloud box running `service`. The producer dials *out* to it. |
| `BBS_PORT` | `7600` | The service's **ingest** port. Must match the service's `-ingest`. |
| `TOKEN` | `cfc29cba…` | Shared secret the producer presents to publish. Must equal the service's `-token`. Generate with `openssl rand -hex 16`. |
| `COLS` / `ROWS` | `80` / `24` | Picture size in character **cells**. 80×24/25 fills a standard BBS screen; bigger looks sharper but only helps callers whose terminals are that large, and costs bandwidth. |
| `TLS` | `1` | `1` wraps the connection in TLS (the service must have `-tls-cert/-tls-key`); `0` is plaintext (and the service must run *without* a cert). |
| `INSECURE` | `1` | With `TLS=1`: `1` skips certificate verification — needed for a self-signed cert. Set `0` once you use a real/Let's-Encrypt cert. |
| `SOURCE` | `camera` | What to capture: `camera` (default webcam), a **device index** like `1` (HDMI capture card / OBS virtual cam), a **file or URL** (`clip.mp4`, `rtsp://…`), `-` (raw stdin from ffmpeg), or `test` (synthetic). |
| `FLIP` | `0` | Horizontal mirror. Default *auto* mirrors only the selfie webcam; set `0` to force off (HDMI/files), `1` to force on. |
| `CAPTION_FILE` | `/tmp/caption.txt` | The producer broadcasts this file's last line as a subtitle bar. Pair with `caption-mic.sh`. Omit to disable captions. |
| `CAPTURE` | `2` | Which audio input `whisper-stream` transcribes (the SDL device index shown in `/tmp/whisper.log`). Omit to use the system default input. |

Render *look* (saturation, contrast, brightness, half-block↔ramp, ramp glyphs) is adjusted **live** with keys in the mirror, or seeded with `--saturation/--contrast/--brightness/--mode/--ramp`. `--in-size WxH` sets the frame size when `SOURCE=-`.

### The door `door.ini`

Set in `door.ini` (re-read per caller, no restart) or pass as `-flags`:

| Key | Default | Meaning |
|-----|---------|---------|
| `encoding` | cp437 | glyph charset: `cp437` (byte `0xDF`) or `utf8` (`▀`) |
| `color` | truecolor | `truecolor` (24-bit) or `16` (CGA palette) |
| `saturation` / `dither` | 1.8 / true | only applied when `color = 16` |
| `fps` | 15 | max frames/sec (the pacer drops below this on slow links) |
| `hint` | true | show a brief "Q/ESC to quit" banner on launch |
| `channel` | cam | which channel to subscribe to |
| `debug` | — | path to log capture-device list + effective fps |

A modern SyncTERM caller usually wants `encoding = cp437`, `color = truecolor`.

### The service (cloud side)

`service` is configured by flags (typically baked into the `ExecStart=` line of
`packaging/telnetvision.service`):

| Flag | Default | Meaning |
|------|---------|---------|
| `-token` | *(required)* | shared secret producers must present — **must equal the producer's `TOKEN`** |
| `-ingest` | `:7600` | where producers connect (open this port to your home IP) |
| `-consumer` | `127.0.0.1:7601` | where doors subscribe — keep it on localhost |
| `-tls-cert` / `-tls-key` | — | TLS cert/key for the ingest listener (omit both for plaintext, then set `TLS=0` on the producer) |

**The token is one shared secret used on both ends.** Generate it once and put the *same* value in both places:

```bash
openssl rand -hex 16
#   cloud:  service -token <value>      (in telnetvision.service ExecStart)
#   home:   TOKEN=<value>               (in .env)
```

If they don't match, the service logs `bad token` and drops the producer. Full daemon/systemd setup is in [packaging/INSTALL.md](packaging/INSTALL.md).

## Video sources

`SOURCE=camera` (default), a device index like `SOURCE=1` (HDMI capture card or OBS virtual camera), a path/URL (`SOURCE=clip.mp4`, loops), or `SOURCE=-` to read raw `bgr24` from stdin:

```bash
ffmpeg -f avfoundation -i "<screen/window/device>" -pix_fmt bgr24 -s 640x480 -f rawvideo - \
  | SOURCE=- IN_SIZE=640x480 ./stream.sh
```

`streamapp.sh "App Name"` launches an app and streams the screen it's on (macOS)

## Live captions

`caption-mic.sh` runs [whisper.cpp](https://github.com/ggerganov/whisper.cpp) on a chosen audio input and writes the current line to `CAPTION_FILE`; the producer broadcasts it.

```bash
brew install whisper-cpp        # provides whisper-stream
./models/download.sh            # base.en model (or: small.en for accuracy)
./live.sh                       # stream + mic captions in one command
```

Pick the audio input with `CAPTURE=<id>` (the device list is logged to `/tmp/whisper.log`). If captions read `[Music]`/`[sound effects]`, you're on the wrong input — see Troubleshooting.

## Building from source

```bash
(cd service && go build -o ../bin/service .)
(cd door    && go build -o ../bin/door .)
```

Go cross-compiles to Linux/macOS/Windows × amd64/arm64; the release workflow builds all six. The door's Unix latency-pacer lives behind build tags ([`io_unix.go`](door/io_unix.go)) and falls back to blocking I/O on Windows ([`io_windows.go`](door/io_windows.go)).

## Project layout

```
producer.py        capture → grade → downscale → push (home)
ascii_cam.py       standalone local viewer (no streaming)
wire.py            wire protocol (framing, FRAME format)
caption_filter.py  whisper-stream output → current-caption file
service/           Go fanout relay (cloud)
door/              Go per-caller renderer (BBS); io_{unix,windows}.go
packaging/         INSTALL.md, door.ini, telnetvision.service (systemd)
models/            download.sh (GGML whisper models; *.bin gitignored)
stream.sh live.sh streamapp.sh caption-mic.sh   launchers
```

## Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| Captions are `[Music]`/`[sound effects]` | whisper is on the wrong audio input — set `CAPTURE=<id>` from the list in `/tmp/whisper.log` |
| No captions at all | mic permission not granted to your terminal (System Settings → Privacy → Microphone) |
| Screen capture is black | DRM-protected content — can't and won't be captured |
| Door view lags / builds up | expected on slow links — the pacer sheds frames; check `effective fps` with `debug =` in door.ini |


## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE).
