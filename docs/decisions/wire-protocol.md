# Decision: Wire protocol (the bytes between peers)

**Status:** Locked
**Date:** 2026-06-03
**Scope:** Everything that flows over the established QUIC connection between
the two peers, after the TLS+PAKE handshake completes.

---

## Design principles

1. **Length-prefixed binary framing, not JSON.** JSON is convenient but heavy
   on the hot path. Use compact binary frames.
2. **Versioned from byte 0.** A one-byte protocol version up front. Future
   evolution is mechanical.
3. **Stream-oriented, not message-oriented.** Use QUIC streams for distinct
   roles (control vs. data) so head-of-line blocking on a chunk doesn't stall
   the control channel.
4. **Each chunk independently verifiable.** Receiver can detect corruption per
   chunk, not only at end-of-file. Enables resume.

## Stream layout

A session uses three QUIC streams:

| Stream ID | Direction | Role |
|---|---|---|
| 0 (bidi) | both | **Control:** session metadata, ACKs, password challenge, completion, errors |
| 4 (uni, sender→receiver) | sender → receiver | **Data:** chunked file bytes |
| 8 (uni, receiver→sender) | receiver → sender | **Receiver-control:** chunk request hints (for resume), progress acks |

Numbering follows QUIC's stream ID convention. Additional streams may be
allocated for parallel directory transfers (see `directory-transfer.md`); in
v1, sequential file transfer over stream 4 is sufficient.

## Common frame format

Every frame on every stream:

```
+--------+--------+--------+--------+--------+--------+----------+
| ver(1) |   type(1)       |       length(uint32 BE)             |
+--------+--------+--------+--------+--------+--------+----------+
|                          payload (length bytes)                |
+----------------------------------------------------------------+
```

- `ver`: protocol version. v1 = `0x01`. Receiver MUST reject frames with
  unknown version after handshake.
- `type`: frame type (see tables below).
- `length`: payload length in bytes. 0 is legal (for some control frames).
- `payload`: type-specific bytes.

Maximum frame payload: **16 MiB** (2^24). Larger payloads must be split.

## Control stream (stream 0) frame types

| Type | Direction | Name | Payload |
|---|---|---|---|
| `0x01` | sender → receiver | `HELLO` | `SenderHello` (see below) |
| `0x02` | receiver → sender | `HELLO_ACK` | `ReceiverHello` |
| `0x03` | sender → receiver | `PASSWORD_CHALLENGE` | 32 bytes: `HMAC-SHA256(password, exporter_key)` |
| `0x04` | receiver → sender | `PASSWORD_RESPONSE` | 32 bytes: receiver's HMAC of typed password |
| `0x05` | sender → receiver | `FILE_INFO` | `FileInfo` (per file in the transfer) |
| `0x06` | receiver → sender | `FILE_ACCEPT` | `FileAcceptDecision` (resume offset, skip, abort) |
| `0x07` | sender → receiver | `TRANSFER_COMPLETE` | empty |
| `0x08` | receiver → sender | `TRANSFER_ACK` | empty |
| `0xFE` | either | `ERROR` | `ErrorFrame` |
| `0xFF` | either | `ABORT` | empty (graceful disconnect signal) |

### Payloads (all encoded as `encoding/gob` for v1 — see "Encoding choice")

```go
type SenderHello struct {
    ProtocolVersion uint8       // 1
    Hostname        string      // sender's hostname (or --name override)
    OS              string      // "linux" | "darwin" | "windows"
    ClientVersion   string      // fsend semver
    TransferKind    uint8       // 1=single-file, 2=multi-file, 3=directory, 4=stdin, 5=text
    TotalFiles      uint32      // count (1 for single, N for multi/dir, 0 for unknown-size stdin)
    TotalBytes      uint64      // sum of file sizes (0 if unknown, e.g. stdin)
    HasPassword     bool        // true → expect PASSWORD_CHALLENGE next
    CompressionHint uint8       // 0=none, 1=zstd-auto-per-chunk
}

type ReceiverHello struct {
    Hostname      string
    OS            string
    ClientVersion string
    Accepts       bool          // false = user rejected at prompt → ABORT follows
}

type FileInfo struct {
    Index       uint32          // 0-based within the transfer
    RelativePath string         // path relative to transfer root (forward slashes, no leading slash)
    Size        uint64          // bytes (0 for empty files)
    Mode        uint32          // unix mode bits (mapped to closest equivalent on Windows)
    ModTime     int64           // unix nanoseconds
    IsDir       bool            // true → no data stream content, just creates the directory
    IsSymlink   bool            // true → SymlinkTarget set, no data stream content
    SymlinkTarget string        // only if IsSymlink
    Blake3Root  [32]byte        // BLAKE3 hash of the full plaintext file (for resume validation)
}

type FileAcceptDecision struct {
    Index       uint32          // matches the FileInfo this responds to
    Action      uint8           // 0=accept-full, 1=resume-from-offset, 2=skip, 3=abort-all
    ResumeOffset uint64         // only when Action=1; must be on a chunk boundary
}

type ErrorFrame struct {
    Code    uint16              // see error catalog below
    Message string              // human-readable
}
```

### Encoding choice

**Use `encoding/gob` for v1 control frames.** Reasons:
- Stdlib; no extra dependency.
- Schema is in-process Go types; refactor-safe.
- Versioned by Go's gob format; backward-compatible field addition is free.
- Fast enough for control frames (microseconds; control is not the hot path).

**Alternative considered:** protobuf or MessagePack. Both add a build-step
or a dependency for a benefit (cross-language interop) we don't need — fsend
talks only to fsend.

**Performance-critical paths** (the chunk stream) use a hand-rolled binary
header, not gob — see next section.

## Data stream (stream 4) frame format

The data stream carries one or more file payloads back-to-back. Each chunk is
a single frame with its own compact header (NOT the gob format above):

```
+--------+--------+--------+--------+--------+--------+--------+--------+
|  type(1) = 0x10  |  flags(1)     |     chunk_length(uint32 BE)         |
+--------+--------+--------+--------+--------+--------+--------+--------+
|     file_index(uint32 BE)       |     chunk_index(uint32 BE)          |
+--------+--------+--------+--------+--------+--------+--------+--------+
|                       blake3_chunk_hash (32 bytes)                    |
+-----------------------------------------------------------------------+
|                       payload (chunk_length bytes)                    |
+-----------------------------------------------------------------------+
```

- `type` = `0x10` (`CHUNK`). All data-stream frames are this type in v1.
- `flags` bits:
  - bit 0 = compressed (1 → payload is zstd-compressed; 0 → raw)
  - bit 1 = last-chunk-of-file (1 → receiver should close+verify the file after this chunk)
  - bits 2-7 = reserved, MUST be 0
- `chunk_length`: bytes in `payload`. Max 1 MiB.
- `file_index`: which `FileInfo` this chunk belongs to.
- `chunk_index`: 0-based within the file.
- `blake3_chunk_hash`: BLAKE3 hash of the *uncompressed* chunk payload.
  Verified by receiver before writing to disk.

**Chunk size:** **1 MiB** (1,048,576 bytes) of plaintext. Last chunk may be
smaller. Trade-off: smaller chunks = finer resume granularity but more header
overhead; larger = better throughput but coarser resume. 1 MiB is the sweet
spot for typical files.

## Receiver-control stream (stream 8) frame format

Same compact header style as data stream. Types:

| Type | Name | Payload |
|---|---|---|
| `0x20` | `PROGRESS_ACK` | `uint32 file_index` + `uint64 bytes_acked` (heartbeat for live progress; sender uses for speed display) |
| `0x21` | `RESUME_REQUEST` | `uint32 file_index` + `uint64 from_offset` (mid-transfer "skip ahead to this offset") |
| `0x22` | `PAUSE` / `0x23` | `RESUME` | empty (receiver-side pause; sender stops sending until resume) |

`PAUSE`/`RESUME` is v2 territory; v1 ignores it.

## Session lifecycle (sequence)

```
   SENDER                        RECEIVER
   ──────                        ────────
   (after QUIC+TLS+PAKE handshake completes, channel binding verified)

   HELLO ────────────────────►
                              ◄──── HELLO_ACK (Accepts=false → ABORT, both close)

   [if HasPassword:]
   PASSWORD_CHALLENGE ───────►
                              ◄──── PASSWORD_RESPONSE
   (sender constant-time-compares; mismatch → ERROR(WRONG_PASSWORD) then ABORT)

   for each file in transfer:
       FILE_INFO ─────────────►
                              ◄──── FILE_ACCEPT(action, resume_offset?)
       [if action == accept or resume:]
           CHUNK frames on stream 4, starting at offset
           ◄── PROGRESS_ACK frames (periodic) on stream 8

   TRANSFER_COMPLETE ────────►
                              ◄──── TRANSFER_ACK
   both sides close cleanly
```

## Error catalog (code → meaning, locked)

| Code | Name | Cause | Action |
|---|---|---|---|
| 1 | `UNSUPPORTED_VERSION` | Protocol version mismatch | Both abort with message |
| 2 | `WRONG_PASSWORD` | Password challenge failed | Sender aborts; receiver shown "wrong password" |
| 3 | `RECEIVER_REJECTED` | User said no at prompt | Sender shows "receiver declined"; both close cleanly |
| 4 | `DISK_FULL` | Receiver out of space | Receiver aborts; sender shown reason |
| 5 | `WRITE_FAILED` | Filesystem error on receiver | Same |
| 6 | `READ_FAILED` | Filesystem error on sender | Sender aborts; receiver shown reason |
| 7 | `CHUNK_HASH_MISMATCH` | A chunk failed BLAKE3 verification | Receiver requests retransmit (v2) or aborts (v1) |
| 8 | `FILE_HASH_MISMATCH` | Full-file BLAKE3 root didn't match | Receiver deletes partial, aborts |
| 9 | `TIMEOUT` | No frames for `--timeout` seconds | Either side aborts |
| 10 | `PROTOCOL_ERROR` | Unexpected frame type or out-of-order | Abort, log details in debug mode |
| 100-199 | Reserved for v2 |
| 200-255 | Vendor-specific |

## Version negotiation

The protocol version is sent in the first byte of every frame (`ver`). The
first `HELLO` carries `ProtocolVersion=1`. Forward-compat rule:

- Receiver checks `HELLO.ProtocolVersion`. If higher than receiver supports,
  receiver sends `ERROR(UNSUPPORTED_VERSION)` and aborts.
- If lower than receiver supports, receiver attempts that older protocol if
  it's still supported.
- v1 sender + v1 receiver: trivially compatible.
- The frame-header `ver` byte must match the negotiated version on every
  subsequent frame.

Future protocols can add new frame types (use unused type bytes) without
bumping the version, as long as the additions are optional (i.e., an old
receiver can ignore unknown control-frame types it doesn't need). Mandatory
changes bump `ver`.

## What is NOT in the wire protocol

- **No authentication of frames at the wire-protocol layer.** The QUIC+TLS
  session already provides confidentiality and integrity for every byte.
  Re-MAC'ing frames here would be wasted CPU.
- **No retries at the wire-protocol layer.** QUIC handles loss + retransmit
  for us. Chunk-level retransmission for hash failures is a v2 concern.
- **No multiplexed concurrent file transfers in v1.** Files go one at a time.
  Concurrent transfers (using one QUIC stream per file) are a v2 optimization.
