# 008 — Wire protocol: length-prefixed frames, fixed little-endian layouts

- **Status:** accepted (milestone 3)

> **Superseded in part (2026-10-14, `bf4c722`):** the gateway has a seventh reject reason, 106 `TooManyRequests`, for market-data requests beyond a connection's rate (review F-005, [decision 016](016-market-data.md)); and since milestone 6 (`26ecd26`) nothing raises 104 `Unsupported`, whose only use was `Subscribe` before market data existed.

- **Code:** `internal/proto/frame.go`, `internal/proto/messages.go`

## Context

PRD 5.5:
- length-prefixed binary frames over TCP;
- a fixed little-endian layout per message type;
- a 1-byte version in the frame header;
- no reflection and no allocation for `NewOrder`, `Ack` and `Fill`;
- no gRPC or protobuf ("the wire protocol is ours").

TCP delivers a **byte stream**, not messages. One `Write` of 37 bytes may arrive as one `Read` of 37 bytes, or as three reads of 10, 20 and 7 bytes, or merged with the next frame. The protocol has to say where each message ends.

## Decisions

### Framing

```
offset  size  field
0       4     length   u32 LE = number of bytes after this field (2 + payload)
4       1     version  must be 1
5       1     type     message type
6       n     payload  fixed layout for the type
```

- **Length prefix** rather than a delimiter (newline) or a self-describing encoding. A reader always knows exactly how many bytes to wait for. With a delimiter it would have to scan every byte, and binary payloads can contain the delimiter.
- **u32 rather than u16.** Two bytes per frame is a small price for never needing a second framing scheme when market-data snapshots (milestone 6) carry a whole book. *(In the end, milestone 6 sends a snapshot as a header plus one fixed-size frame per level, so frames stay small; [decision 016](016-market-data.md).)* Size is still bounded by `MaxPayload`, below.
- **The length counts version and type** as well as the payload, so the length alone says how far to skip.
- **Version in every frame**, not only at login: it costs one byte and makes every frame self-checking. A frame with any version other than 1 closes the connection.

### Bounding the length (`MaxPayload = 1024`)

The length field comes from the network. Without a bound, a corrupt or hostile frame claiming 4 GB would make the server try to buffer 4 GB. The reader rejects any frame whose payload is over 1,024 bytes before reading it. The largest current message is 70 bytes. Milestone 6 kept it: snapshots are sent one level per frame, so no message grew.

### Fixed layouts, exact sizes

Each message type has exactly one payload size, and `Decode` rejects any other length (`ErrPayloadSize`). There are no optional fields and no variable-length fields. That gives:
- decoding with fixed offsets, no allocation, no branching on content;
- a one-to-one mapping between messages and bytes. The fuzz test checks this: anything that decodes must re-encode to the identical bytes.

**Little-endian** because x86-64 and ARM64, where this runs, are little-endian, so encoding is a plain store. NASDAQ's ITCH and OUCH use big-endian ("network byte order"); we don't need to interoperate with them.

There is no padding and no alignment. Fields are read with `binary.LittleEndian.Uint64(b[off:])`, which handles unaligned data.

### Messages

| type | code | direction | payload bytes |
|---|---|---|---:|
| Login | 0x01 | in | 4 |
| NewOrder | 0x02 | in | 31 |
| Cancel | 0x03 | in | 12 |
| Subscribe | 0x04 | in | 4 |
| Heartbeat | 0x05 | both | 0 |
| LoginAck | 0x81 | out | 4 |
| Ack | 0x82 | out | 53 |
| Reject | 0x83 | out | 37 |
| Fill | 0x84 | out | 70 |
| CancelAck | 0x85 | out | 54 |
| Resend | 0x06 | in | 12 (milestone 6) |
| BookDelta | 0x86 | out | 45 (milestone 6) |
| Snapshot | 0x87 | out | 16: header; levels follow (milestone 6) |
| Trade | 0x88 | out | 53 (milestone 6) |
| SnapshotLevel | 0x89 | out | 21 (milestone 6) |

*(Milestone 8, additive, no version bump:* TIF `IOC` = 3 and `FOK` = 4, limit-only; cancel reason `IOCUnfilled` = 3; reject reason `FOKNoLiquidity` = 11. [Decision 020](020-ioc-and-fok.md).) *(Review F-005, additive:* gateway reject reason `TooManyRequests` = 106, for market-data requests beyond a connection's rate; [decision 016](016-market-data.md).)

The field-by-field layouts are in the doc comment on each type in `messages.go`. **`LoginAck` is an addition to PRD 5.5's list**: nothing in that list answers a successful login. Without it, a client couldn't tell "logged in" from "the server hasn't answered yet".

The outbound messages map one-to-one onto engine events ([decision 006](006-event-model.md)): Ack, Reject, Fill, and `CancelAck` for requested cancels, expired market remainders and (since milestone 8) unfilled IOC remainders, with a reason byte. Every outbound message carries the command's `seq` and `time`.

### Enum values belong to the wire

Side, type, TIF, role, cancel reason and reject reason are defined in `proto` with explicit numbers. They currently equal the engine's numbers, so the gateway converts by casting. `TestEnumsMatchWire` asserts that they match, value by value and name by name. If the engine ever renumbers an enum, the test fails instead of the protocol silently changing.

**Reject reasons:**
- 1–99 are the engine's reasons, which are sequenced and logged;
- 100 and up are the gateway's, raised before sequencing (`NotLoggedIn`, `AlreadyLoggedIn`, `ClientInUse`, `InvalidClientID`, `Unsupported` (not raised since milestone 6), since milestone 6 `ResendUnavailable`, and since review F-005 `TooManyRequests`), and always carry `seq = 0`.

### Hand-written codec

Encoders are `AppendX(dst []byte, m *X) []byte`: they append the frame to a caller's buffer. Decoders are `(m *X) Decode(payload []byte) error`: they fill a value in place.

This uses no reflection and no `encoding/binary.Read` (which uses reflection and allocates). Measured: 8–18 ns per operation and 0 allocations for all six hot operations (`docs/guide/03-gateway.md`).

`proto.Reader` reads the 6-byte header into a fixed array and the payload into one reused 1,024-byte buffer. The payload slice it returns is valid only until the next call, a rule written on the method.

## Costs

- **Fixed layouts mean every format change is a new version or a new type.** There is no way to add an optional field to an existing message. That is deliberate, and the version byte is the escape hatch.
- **No checksum in the frame.** TCP already checksums segments, and the WAL (milestone 4) has its own CRC. A frame-level CRC would only catch memory corruption between the socket and the decoder.
- **Little-endian is non-standard for network protocols.** Anyone writing a client in another language must read the layout tables, not assume network byte order.
- **No resynchronisation.** After a framing error the stream position is unknown, so the connection is closed. The client must reconnect.

## How it's tested

- **Round trip:** every type with random values, 200 frames each, sent through the reader one byte at a time.
- **Golden bytes** for `NewOrder`, pinning the documented offsets. A planted bug that swapped `price` and `qty` in *both* the encoder and the decoder passed the round-trip test, and only the golden test caught it.
- **Wrong sizes** for every type; every reader error (bad length, bad version, truncated header, truncated payload, clean EOF); the `MaxPayload` boundary exactly.
- **`FuzzReader`:** about 5.9M executions in 60 s with no failures.
- **Allocations:** `TestZeroAllocCodec`, with both `AllocsPerRun` and the per-stack check from [decision 007](007-allocation-testing.md).
