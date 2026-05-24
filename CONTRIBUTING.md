# Contributing to Telnetvision

Thanks for the interest! Telnetvision is a small, hackable project — patches, bug reports, and new render modes / sources are all welcome.

## Scope & acceptable use

Telnetvision streams content **you own or are authorized to broadcast**. Please don't propose features whose primary purpose is to **capture or rebroadcast DRM-protected or licensed commercial streams** (e.g. defeating HDCP/Widevine, the screen-capture blackout, etc.) — those won't be merged. Everything else about making webcams, game consoles, your own media, and BBS terminals do delightful things is fair game.

## Project layout

| Path | Language | Role |
|------|----------|------|
| `producer.py`, `wire.py`, `ascii_cam.py` | Python | home capture/grade/push; wire protocol; standalone viewer |
| `caption_filter.py`, `caption-mic.sh` | Python/sh | whisper.cpp mic captions |
| `service/` | Go | fanout relay (cloud) |
| `door/` | Go | per-caller renderer (BBS); platform I/O in `io_unix.go` / `io_windows.go` |
| `packaging/` | — | Synchronet install docs, `door.ini`, systemd unit |
| `*.sh` | sh | launchers (`stream.sh`, `live.sh`, `streamapp.sh`) |

The wire format (framing, the `FRAME` message) is documented at the top of [`wire.py`](wire.py); keep `wire.py` and the Go parsers in `service/`/`door/` in sync when you change it.

## Dev setup

```bash
python3 -m venv .venv && ./.venv/bin/pip install -r requirements.txt
# Go 1.25+ for service/ and door/
```

## Run the whole pipeline locally (no camera, no BBS)

Three terminals, using the synthetic `test` source so you don't need a webcam or a Synchronet box:

```bash
# 1. the relay
go -C service run . -token dev

# 2. a producer (synthetic source, headless so it doesn't take over the terminal)
./.venv/bin/python producer.py --source test --token dev --no-mirror

# 3. view it as a "caller" (truecolor UTF-8 for a modern terminal; q to quit)
go -C door run . -encoding utf8 -color truecolor
```

`--source test` is also handy in CI/automated checks: pass `--frames N` to the producer or door to exit after N frames. For the local mirror + key controls, run the producer in a terminal *without* `--no-mirror`.

## Conventions

- **Match the surrounding style.** Go is `gofmt`'d and `go vet`-clean (`go -C door vet ./...`). Python targets the stdlib + `opencv-python`/`numpy`, no extra deps without good reason.
- **Keep the door cross-platform.** Anything OS-specific (non-blocking I/O, raw mode, console handles) belongs behind build tags in `io_unix.go` / `io_windows.go`, not in `main.go`. Before sending a door change, confirm it still cross-compiles:
  ```bash
  for t in linux/amd64 darwin/arm64 windows/amd64; do
    GOOS=${t%/*} GOARCH=${t#*/} CGO_ENABLED=0 go -C door build -o /dev/null . && echo "ok $t"
  done
  ```
- **Don't commit secrets or build output.** `.env`, `.venv/`, `bin/`, `dist/`, and `models/*.bin` are gitignored — keep them that way.
- **Prefer freshness and bounded latency.** The door drops stale frames rather than queueing them; new render paths should fit that model (delta-encode, render the latest).

## Pull requests

- Keep PRs focused; describe what you changed and how you tested it.
- CI (`.github/workflows/ci.yml`) cross-compiles every target and runs `go vet` — make sure it's green.
- For protocol or render changes, note the before/after behavior (a quick byte-count or screenshot helps).

## Releases (maintainers)

Tag a version and push it; `.github/workflows/release.yml` builds and attaches the per-platform bundles:

```bash
git tag v0.1.0 && git push origin v0.1.0
```
