"""Producer-side wire protocol (version 2).

Framing: every message is `uint32 length (big-endian)` + payload.
payload[0] is the message type.

  HELLO_PRODUCER 0x01: 0x01 | u8 tokenLen | token | u8 chanLen | channel
                       | [u8 caps]  (v2, optional: bit0 = sends AUDIO)
  HELLO_CONSUMER 0x02: 0x02 | u8 chanLen | channel        (consumer side)
                       | [u8 caps]  (v2, optional: bit0 = wants AUDIO)
  FRAME          0x10: 0x10 | u16 cols | u16 rows | u8 mode | u8 ramp
                       | u16 capLen | caption[capLen] (UTF-8) | pixels[2*rows*cols*3]
                       | [u64 pts_ms]  (v2, optional trailing extension)
                       full-color RGB, row-major; (2*rows) pixel rows so each
                       cell holds a top and bottom pixel.
                       mode: 0=half-block, 1=ramp (glyph by brightness)
                       ramp: 0=ascii " .:-=+*#%@", 1=shades " ░▒▓█" (used when mode=1)
                       caption: optional subtitle text the door draws as a bottom row
                       pts_ms: capture time on the producer's monotonic clock,
                       shared with AUDIO pts so the door can A/V-sync. Appended
                       AFTER the pixels because v1 doors slice pixels by length
                       and ignore trailing bytes — old doors keep working.
                       The door still decides truecolor vs 16-color + charset.
  AUDIO          0x11: 0x11 | u8 codec | u8 channels | u32 sample_rate
                       | u32 seq | u64 pts_ms | u32 dur_ms | payload
                       codec: 0=raw S16LE | 1=FLAC container | 2=WAV container
                       payload is OPAQUE to the relay; only the door interprets
                       it (and even the door never decodes FLAC — it forwards
                       the container to the caller's terminal). dur_ms is the
                       chunk's play time, declared here so the door can run its
                       queue ledger without parsing the codec.

Version notes: v2 adds AUDIO, the FRAME pts trailer and the hello caps byte.
All three are optional trailing bytes that v1 peers already ignore, so v1
doors work against v2 producers (they just never see audio).
"""
import struct

MSG_HELLO_PRODUCER = 0x01
MSG_HELLO_CONSUMER = 0x02
MSG_FRAME = 0x10
MSG_AUDIO = 0x11

CAP_AUDIO = 0x01

AUDIO_CODEC_RAW = 0   # raw S16LE interleaved PCM
AUDIO_CODEC_FLAC = 1  # FLAC container
AUDIO_CODEC_WAV = 2   # WAV (RIFF) container


def send_msg(sock, payload: bytes) -> None:
    sock.sendall(struct.pack(">I", len(payload)) + payload)


def hello_producer(sock, token: str, channel: str, caps: int = 0) -> None:
    t = token.encode()
    c = channel.encode()
    if len(t) > 255 or len(c) > 255:
        raise ValueError("token/channel must be <= 255 bytes")
    payload = bytes([MSG_HELLO_PRODUCER, len(t)]) + t + bytes([len(c)]) + c
    if caps:
        payload += bytes([caps & 0xFF])
    send_msg(sock, payload)


def frame_payload(cols: int, rows: int, pixels: bytes, mode: int = 0, ramp: int = 0,
                  caption: bytes = b"", pts_ms: int = None) -> bytes:
    caption = caption[:65535]
    payload = (bytes([MSG_FRAME]) + struct.pack(">HHBB", cols, rows, mode, ramp)
               + struct.pack(">H", len(caption)) + caption + pixels)
    if pts_ms is not None:
        payload += struct.pack(">Q", pts_ms)
    return payload


def audio_payload(codec: int, channels: int, sample_rate: int, seq: int,
                  pts_ms: int, dur_ms: int, payload: bytes) -> bytes:
    return (bytes([MSG_AUDIO, codec & 0xFF, channels & 0xFF])
            + struct.pack(">IIQI", sample_rate, seq & 0xFFFFFFFF, pts_ms, dur_ms)
            + payload)
