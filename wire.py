"""Producer-side wire protocol.

Framing: every message is `uint32 length (big-endian)` + payload.
payload[0] is the message type.

  HELLO_PRODUCER 0x01: 0x01 | u8 tokenLen | token | u8 chanLen | channel
  HELLO_CONSUMER 0x02: 0x02 | u8 chanLen | channel        (consumer side)
  FRAME          0x10: 0x10 | u16 cols | u16 rows | u8 mode | u8 ramp
                       | u16 capLen | caption[capLen] (UTF-8) | pixels[2*rows*cols*3]
                       full-color RGB, row-major; (2*rows) pixel rows so each
                       cell holds a top and bottom pixel.
                       mode: 0=half-block, 1=ramp (glyph by brightness)
                       ramp: 0=ascii " .:-=+*#%@", 1=shades " ░▒▓█" (used when mode=1)
                       caption: optional subtitle text the door draws as a bottom row
                       The door still decides truecolor vs 16-color + charset.
"""
import struct

MSG_HELLO_PRODUCER = 0x01
MSG_HELLO_CONSUMER = 0x02
MSG_FRAME = 0x10


def send_msg(sock, payload: bytes) -> None:
    sock.sendall(struct.pack(">I", len(payload)) + payload)


def hello_producer(sock, token: str, channel: str) -> None:
    t = token.encode()
    c = channel.encode()
    if len(t) > 255 or len(c) > 255:
        raise ValueError("token/channel must be <= 255 bytes")
    payload = bytes([MSG_HELLO_PRODUCER, len(t)]) + t + bytes([len(c)]) + c
    send_msg(sock, payload)


def frame_payload(cols: int, rows: int, pixels: bytes, mode: int = 0, ramp: int = 0,
                  caption: bytes = b"") -> bytes:
    caption = caption[:65535]
    return (bytes([MSG_FRAME]) + struct.pack(">HHBB", cols, rows, mode, ramp)
            + struct.pack(">H", len(caption)) + caption + pixels)
