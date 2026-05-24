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
- `color` — `truecolor` (24-bit) or `16` (CGA palette)

A modern SyncTERM caller usually wants `encoding = cp437` **and** `color = truecolor`.
For maximum compatibility with old clients, use `color = 16`.

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
