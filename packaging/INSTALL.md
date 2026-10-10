# Telnetvision — Synchronet (Linux) install

Three pieces:

```
[home]  producer.py  ──outbound TLS──▶  [cloud]  service  ──localhost──▶  door (xtrn) ──▶ SyncTERM caller
 (captures source,                      (fanout relay,                    (per caller,
  pushes out)                            :7600 ingest / :7601 local)       renders CP437/ANSI)
```

Only `service` and `door` run on the BBS box; `producer.py` stays on your home machine.
This guide is Linux/Debian; the release bundles also include macOS and Windows builds
(Synchronet runs on Windows too — same steps, Windows paths).

---

## 1. Pick the right binaries
On the Debian box:

```
uname -m      # x86_64 -> use linux-amd64/   |   aarch64 -> use linux-arm64/
```

## 2. Install the binaries
```
sudo mkdir -p /sbbs/xtrn/telnetvision
sudo cp linux-amd64/service linux-amd64/door /sbbs/xtrn/telnetvision/    # or linux-arm64/
sudo cp door.ini /sbbs/xtrn/telnetvision/                               # door config (editable, no restart)
sudo chmod +x /sbbs/xtrn/telnetvision/service /sbbs/xtrn/telnetvision/door
```

## 3. Token + TLS cert
```
openssl rand -hex 16        # your shared token — copy it for steps 4 and 7

# self-signed quickstart (or drop in a real cert / Let's Encrypt fullchain+key)
sudo openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -keyout /sbbs/xtrn/telnetvision/key.pem \
  -out    /sbbs/xtrn/telnetvision/cert.pem \
  -subj "/CN=YOUR.BBS.HOSTNAME"
```

## 4. Run the service (systemd)
Edit `telnetvision.service` (token, user, paths), then:
```
sudo cp telnetvision.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now telnetvision
sudo systemctl status telnetvision    # expect: ingest on :7600 (tls=true), consumers on 127.0.0.1:7601
```

## 5. Firewall
Open **only** 7600 inbound, ideally just from your home IP. 7601 is bound to
127.0.0.1 and must stay private.
```
sudo ufw allow from YOUR.HOME.IP to any port 7600 proto tcp
```

## 6. Synchronet door config (`scfg`)
External Programs → Online Programs (Doors) → (a section) → add a program:

| Field                 | Value                          |
|-----------------------|--------------------------------|
| Name                  | `Telnetvision`                 |
| Internal Code         | `TVISION`                      |
| Start-up Directory    | `/sbbs/xtrn/telnetvision`          |
| Command Line          | `door`                         |

Then in the program's options/toggles:

- **Native Executable:** Yes
- **Intercept Standard I/O:** Yes  *(Synchronet relays the door's stdout to the caller and the caller's keys to stdin)*
- **Multiple Concurrent Users:** Yes  *(several callers can watch the same stream — that's the whole point of the fanout)*
- **BBS Drop File Type:** None
- **Pause After Execution:** No

Save, exit `scfg`, and recycle the BBS (or it's picked up on the next run).
The door reads its config from `door.ini` in the Start-up Directory, so the command
line is just `door` — no host/port/flags needed.

### Tuning without a restart: `door.ini`
The door re-reads `door.ini` every time a caller launches it, so edits apply to the
**next caller with no Synchronet restart** (existing sessions keep their settings).

Two **independent** settings (glyph charset vs color depth):

- `encoding` — `cp437` (byte 0xDF, what SyncTERM expects) or `utf8` (`▀`, for UTF-8 terminals)
- `color` — `auto` (ask the terminal; default), `truecolor` (force 24-bit) or `16` (force the CGA palette); `shade` — `true` (default) renders the 16-color path with perceptual shade glyphs, `false` keeps dithered half blocks

With `color = auto` a SyncTERM or fTelnet caller gets 24-bit and a NetRunner caller gets 16 colors without any setting. Use `truecolor` or `16` only to force one depth for everybody.

Also `channel`, `saturation`, `dither` (color=16 only), `fps`, and `host`/`port`. Any
CLI flag overrides the file (handy for a quick test without editing the ini).

## 7. Stream from your Mac
```
./.venv/bin/python producer.py --source camera \
  --host YOUR.BBS.HOSTNAME --port 7600 --token YOUR_TOKEN \
  --tls --insecure \          # drop --insecure once you use a real cert
  --channel cam --cols 80 --rows 24
```
80×24 = a standard BBS screen. The producer dials out, so your home network needs
no inbound ports.

## 8. Test
Connect to the BBS in SyncTERM and launch **Telnetvision** from the doors menu.
`Q`, `q`, `ESC` (or Ctrl-C / disconnect) exits the door back to the BBS.

---

## Notes & known limits
- **iCE color:** `-cp437` currently uses the 8 dark ANSI backgrounds, so bottom-half
  pixels lose their bright variants. Enabling iCE color (16 backgrounds) is a planned tweak.
- **One producer = one resolution**, broadcast to all callers. Push 80×24 for BBS
  callers. (Door-side autoscaling — one HD stream serving every terminal size — is the
  planned fix.) The multi-channel design also lets you run separate channels later.
- **Look tuning** (only affects `-cp437`): `door -saturation 2.2`, `door -dither=false`.
- If the door exits the instant a caller launches it, that's stdin-EOF handling under
  Synchronet's I/O intercept — flag it and it's a quick fix.

---

## Other BBS packages (DOOR32.SYS)

For BBSes outside the Synchronet family — EleBBS, RemoteAccess, Mystic, MagickaBBS, anything in the RA lineage — the door auto-detects a `DOOR32.SYS` dropfile in its working directory at launch. When line 1 is `2` (telnet socket), the door reads and writes the already-open socket the BBS inherited to it (handle on line 2) instead of stdin/stdout. No stdio-intercept toggle, no telnet redirector, no flags needed.

**Setup in your BBS's external-program config:**

| Field | Value |
|---|---|
| Working directory | the door's install folder (where `door` / `door.exe` + `door.ini` live) |
| Dropfile format | `DOOR32.SYS` — if your BBS offers a choice, pick this one |
| Command line | `door` (or `door.exe`) — no flags needed; the dropfile is auto-detected |
| Stdio intercept toggle | leave OFF if your BBS has one (irrelevant in socket mode) |
| Multi-node | allowed — each caller spawns a fresh process with their own socket |

The door's `host`/`port`/`channel`/`encoding`/`color` settings still come from `door.ini` exactly as they do under Synchronet — the only thing DOOR32.SYS mode changes is *where the caller's bytes flow*. Pass `-door32 ""` to force stdio mode for testing.

### Tested combinations

- **Synchronet (stdio path)** — primary deployment, exercised in the steps above.
- **DOOR32.SYS socket inheritance** — implemented to the RA-published spec and verified on Linux/macOS with a synthetic harness that feeds an inherited Unix socket through end-to-end (auto-detect → wrap → render to socket → input from socket). **First real-world integration target: EleBBS on Windows.** Windows uses Go's `os.NewFile` to wrap the SOCKET handle, which works for plain blocking sockets (what `accept()` returns by default) but may need adjustment for BBSes that pass `WSA_FLAG_OVERLAPPED` sockets. If launch fails with I/O errors in the door's `debug` log, file an issue with the BBS package name + version + a redacted copy of the `DOOR32.SYS` your BBS dropped.
