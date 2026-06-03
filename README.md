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

#### Home — on Windows (32-bit or 64-bit)

The producer is Python and runs on Windows; the bash launchers (`stream.sh`, etc.) don't, so you invoke `producer.py` directly. Steps from a fresh Windows box:

1. **Install Python 3** for Windows — the official installer (`x86` for 32-bit Windows, `x64` for 64-bit) from <https://www.python.org/downloads/windows/>. Tick **"Add Python to PATH"** in the installer.
2. **Install Git for Windows** (matches your bitness) — for `git clone`. It also includes Git Bash, in case you'd rather run the `.sh` launchers.
3. Clone, set up the venv, install deps:
   ```cmd
   git clone https://github.com/hmderdoc/telnetvision.git
   cd telnetvision
   py -m venv .venv
   .venv\Scripts\activate
   pip install -r requirements.txt
   copy .env.example .env
   ```
   Then edit `.env` and fill in `BBS_HOST`, `BBS_PORT`, `TOKEN`. **On 32-bit Windows** specifically, pip may not find wheels for the latest `opencv-python`/`numpy` — pin a known-compatible older combo:
   ```cmd
   pip install "opencv-python==4.8.1.78" "numpy<2"
   ```
4. Run the producer (one line; flags map 1:1 to `.env` keys, since `stream.sh`'s `.env` loader is bash):
   ```cmd
   python producer.py --host YOUR_BBS_HOST --port 7600 --token YOUR_TOKEN ^
       --channel cam --cols 80 --rows 24 --tls --insecure --source camera
   ```
   The live mirror (the `+/- m g q` keys) is **auto-disabled on Windows** because it relies on Unix TTY APIs — the stream itself works fine without it.

For live mic captions on Windows you also need **whisper.cpp's `whisper-stream.exe`**. Grab a prebuilt zip from <https://github.com/ggml-org/whisper.cpp/releases> — pick `whisper-blas-bin-Win32.zip` (32-bit) or `whisper-blas-bin-x64.zip` (64-bit). Extract it; the zip ships `whisper-stream.exe`, `SDL2.dll`, and a ggml model is downloaded separately. Grab a model the same way the Mac/Linux side does — `ggml-base.en.bin` from <https://huggingface.co/ggerganov/whisper.cpp> works.

`caption-mic.sh` is bash (run it under Git Bash if you'd rather), but you can wire whisper directly without it — the Python filter is cross-platform:
```cmd
whisper-stream.exe -m models\ggml-base.en.bin -c -1 -t 6 ^
    --step 700 --length 5000 --keep 200 2>whisper.log ^
    | python caption_filter.py C:\Temp\caption.txt
```
Then run the producer with `--caption-file C:\Temp\caption.txt` (or set `CAPTION_FILE` in your environment). Pick a specific input device with `-c <SDL index>` — the indices are listed in `whisper.log` after startup (look for `Capture device #N: '<name>'`).

> **The `service`/`door` binaries** build for Linux, macOS, Windows, FreeBSD, OpenBSD × amd64/arm64 (plus 386 on Linux/Windows). They run on the BBS box — see below.

### BBS / cloud side

Download the release bundle for your OS/arch (or build — see below) and follow **[packaging/INSTALL.md](packaging/INSTALL.md)**. In short: run `service` as a daemon (listens for the producer on `:7600`, for doors on `127.0.0.1:7601`), open only `:7600` inbound, and add `door` as an external program in your BBS.

Two integration paths, both work without rebuilding:

- **Synchronet** — add the door with *Intercept Standard I/O = Yes* and *Multiple Concurrent Users = Yes*. The BBS bridges the door's stdin/stdout to the caller's connection. Full steps in INSTALL.md §6.
- **RA-family BBSes** (EleBBS, RemoteAccess, Mystic, MagickaBBS, …) — configure the door as a regular external program with `DOOR32.SYS` as the dropfile format. The door auto-detects the dropfile, picks up the inherited socket handle (DOOR32.SYS line 2 when line 1 is `2`/telnet), and talks to it directly. No stdio intercept needed. Details in INSTALL.md §"Other BBS packages (DOOR32.SYS)".

## Configuration

### The producer `.env`

`stream.sh` (and `live.sh`) read settings from a `.env` file in the project root — copy `.env.example` to `.env` and edit. Every key can also be given as a one-off override on the command line (`COLS=120 ./stream.sh`) or as a producer flag (`./stream.sh --cols 120`); an explicit override wins over `.env`. A typical `.env`:

```ini
# Where the relay lives, and the shared secret to publish to it.
BBS_HOST=your.bbs.hostname     # the cloud box running `service`
BBS_PORT=7600                  # its ingest port (matches service -ingest)
TOKEN=cfc29cba...              # matches service -token; make one: openssl rand -hex 16

# Channel name. Must match the door's `channel=` in door.ini (both default `cam`).
CHANNEL=cam                    # change to run a second feed (e.g. CHANNEL=desk)

# How big a picture to send (in terminal cells; a BBS screen is 80x24/25).
COLS=80
ROWS=24

# Transport security.
TLS=1                          # 1 = encrypt (service needs a cert); 0 = plaintext
INSECURE=1                     # 1 = don't verify the cert (self-signed); 0 = verify (real cert)

# Optional extras (safe to omit):
CAPTION_FILE=/tmp/caption.txt  # broadcast this file's last line as a subtitle
# SOURCE=camera                # what to capture (see below)
# CAPTURE=2                    # which audio input to transcribe — index OR name (see Live captions)
# FLIP=0                       # mirror off (auto-mirrors only the default webcam)
```

| Key | Example | What it does |
|-----|---------|--------------|
| `BBS_HOST` | `futureland.today` | Hostname/IP of the cloud box running `service`. The producer dials *out* to it. |
| `BBS_PORT` | `7600` | The service's **ingest** port. Must match the service's `-ingest`. |
| `TOKEN` | `cfc29cba…` | Shared secret the producer presents to publish. Must equal the service's `-token`. Generate with `openssl rand -hex 16`. |
| `CHANNEL` | `cam` | Channel name the producer publishes to. The service routes by channel: one publisher per channel, many door subscribers. **Must match the door's `channel=` in `door.ini`** (also `cam` by default). Use a different name to run a second feed alongside (e.g. `CHANNEL=desk` for a screencap channel, with a second door pointed at it). |
| `COLS` / `ROWS` | `80` / `24` | Picture size in character **cells**. 80×24/25 fills a standard BBS screen; bigger looks sharper but only helps callers whose terminals are that large, and costs bandwidth. |
| `TLS` | `1` | `1` wraps the connection in TLS (the service must have `-tls-cert/-tls-key`); `0` is plaintext (and the service must run *without* a cert). |
| `INSECURE` | `1` | With `TLS=1`: `1` skips certificate verification — needed for a self-signed cert. Set `0` once you use a real/Let's-Encrypt cert. |
| `SOURCE` | `camera` | What to capture: `camera` (default webcam), a **device index** like `1` (HDMI capture card / OBS virtual cam), a **file or URL** (`clip.mp4`, `rtsp://…`), `-` (raw stdin from ffmpeg), or `test` (synthetic). |
| `FLIP` | `0` | Horizontal mirror. Default *auto* mirrors only the selfie webcam; set `0` to force off (HDMI/files), `1` to force on. |
| `CAPTION_FILE` | `/tmp/caption.txt` | The producer broadcasts this file's last line as a subtitle bar. Pair with `caption-mic.sh`. Omit to disable captions. |
| `CAPTURE` | `2` or `"usb audio"` | Which audio input `whisper-stream` transcribes. Either the integer SDL device index, or a **case-insensitive device-name substring** (resolved at startup — survives reorderings when AirPods or other inputs toggle). Run `LIST=1 ./caption-mic.sh` to dump the current devices. Omit to use the system default. |

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

`SOURCE=camera` (default), a device index like `SOURCE=1` (HDMI capture card or OBS virtual camera), a path/URL (`SOURCE=clip.mp4`, loops), or `SOURCE=-` to read raw `bgr24` from stdin. The stdin route lets any `ffmpeg` capture feed the producer — the input flag is OS-specific:

```bash
# macOS:    -f avfoundation -i "<screen/device index>"
# Linux:    -f x11grab -i :0.0       (screen)  |  -f v4l2 -i /dev/video0   (webcam)
# Windows:  -f gdigrab -i desktop    (screen)  |  -f dshow -i video="..."  (device)
ffmpeg <input flags above> -pix_fmt bgr24 -s 640x480 -f rawvideo - \
  | SOURCE=- IN_SIZE=640x480 ./stream.sh
```

`streamapp.sh "App Name"` launches an app and streams the screen it's on (macOS-only — it uses avfoundation screen capture).

## Live captions

`caption-mic.sh` runs [whisper.cpp](https://github.com/ggerganov/whisper.cpp) on a chosen audio input and writes the current line to `CAPTION_FILE`; the producer broadcasts it.

First install whisper.cpp's `whisper-stream`:
- **macOS:** `brew install whisper-cpp`
- **Linux:** your distro package if available, otherwise build from [whisper.cpp](https://github.com/ggml-org/whisper.cpp) (`cmake -B build -DWHISPER_SDL2=ON && cmake --build build`)
- **Windows (32 or 64-bit):** grab a prebuilt zip from the [whisper.cpp releases](https://github.com/ggml-org/whisper.cpp/releases) — use `whisper-blas-bin-Win32.zip` for 32-bit Windows or `whisper-blas-bin-x64.zip` for 64-bit. (The `blas` variants are BLAS-accelerated and faster than plain.) Extract; the zip contains `whisper-stream.exe`, `SDL2.dll`, and the other tools — no compile needed. Add the `Release\` folder to your `PATH` (or call `whisper-stream.exe` by full path).

```bash
./models/download.sh            # base.en model (or: small.en for accuracy)
./live.sh                       # stream + mic captions in one command (bash; macOS/Linux)
```

Pick the audio input with `CAPTURE=<index>` or `CAPTURE="<name substring>"` — the latter is **resilient when devices shuffle** (e.g. AirPods toggling reorders SDL indices), at the cost of a ~10s discovery on startup. `LIST=1 ./caption-mic.sh` dumps the current device list cleanly so you don't have to fish through `/tmp/whisper.log`. If captions read `[Music]`/`[sound effects]`, you're on the wrong input — see Troubleshooting.

### Captioning the source's own audio (no mic)

When `SOURCE` is a URL or file that carries its own audio — HDHomeRun MPEG-TS, RTSP cameras, mp4 files, anything ffmpeg can read — [`caption-source.py`](caption-source.py) demuxes that audio, chunks it, runs `whisper-cli` per chunk, and writes the cleaned transcription into `CAPTION_FILE`. The producer is already broadcasting that file, so nothing on the producer side has to change. This is the path to use when there's no local mic, or when you want to caption the *content* rather than the broadcaster.

Prerequisites: `ffmpeg` and `whisper-cli` on `PATH` (the prebuilt whisper.cpp Windows zip ships `whisper-cli.exe` alongside `whisper-stream.exe`), and a model under `models/`.

```bash
SOURCE=http://192.168.0.23:5005/auto/v2.1 python caption-source.py
SOURCE=clip.mp4 CAPTION_MODEL=small.en CHUNK_SECS=8 python caption-source.py
```

Latency is roughly `CHUNK_SECS` + transcribe time (~1–3s on CPU for `base.en`), so 5–8s end-to-end — fine for BBS captioning. Larger chunks = more accurate but laggier; smaller = snappier but choppier mid-sentence.

Note: the producer is also consuming `SOURCE` for video. Most HTTP/RTSP endpoints allow concurrent consumers; some HDHomeRun configurations don't — if you hit "device busy" errors, point one of them at a local tee or recording instead.

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
caption_filter.py  shared text-cleaning (drops [Music], ANSI, timestamps...)
caption-mic.sh     whisper-stream on local mic → CAPTION_FILE
caption-source.py  ffmpeg-demux SOURCE audio → whisper-cli chunks → CAPTION_FILE
service/           Go fanout relay (cloud)
door/              Go per-caller renderer (BBS); io_{unix,windows}.go
packaging/         INSTALL.md, door.ini, telnetvision.service (systemd)
models/            download.sh (GGML whisper models; *.bin gitignored)
stream.sh live.sh streamapp.sh caption-mic.sh   launchers
```

## Troubleshooting

| Symptom | Likely cause / fix |
|---------|--------------------|
| Captions are `[Music]`/`[sound effects]` | whisper is on the wrong audio input — run `LIST=1 ./caption-mic.sh` to see devices, then `CAPTURE=<index>` or `CAPTURE="<name>"` to pin it |
| No captions at all | mic permission not granted to your terminal (System Settings → Privacy → Microphone) |
| Screen capture is black | DRM-protected content — can't and won't be captured |
| Door view lags / builds up | expected on slow links — the pacer sheds frames; check `effective fps` with `debug =` in door.ini |


## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

[MIT](LICENSE).
