# Decision: Relay protocol (UDP fallback path)

**Status:** Locked
**Date:** 2026-06-03
**Scope:** How `fsend-server` forwards UDP datagrams between two peers when
ICE hole-punching has failed. Does NOT cover the HTTP signaling path — see
`signaling-protocol.md`.

---

## Design principles

1. **The relay is a blind UDP pipe.** It demuxes datagrams to the right
   session but never enters the TLS/QUIC session — peers' end-to-end
   encryption is preserved.
2. **One UDP port handles all concurrent relayed sessions.** Demux by a small
   per-datagram prefix, not by port allocation.
3. **Session tokens are opaque to clients.** Server issues them; clients
   prepend them to every datagram; server uses them to look up the peer.
4. **Byte counting must work without TLS introspection.** The relay counts
   wire bytes per session for the relay-bytes-per-session cap.
5. **No state for individual datagrams.** Forwarding is a hot-path table
   lookup + write. No per-packet logging in normal operation.

## Datagram format on the wire

Every datagram a client sends to the relay's UDP port:

```
+--------+--------+--------+--------+--------+--------+--------+--------+
|  v(1)  |     session_token (16 bytes, fixed-width, base32-decoded)    |
+--------+--------+--------+--------+--------+--------+--------+--------+
|        ... (token continued) ...                                      |
+--------+--------+--------+--------+--------+--------+--------+--------+
|                              payload                                  |
|              (the peer's QUIC datagram, opaque to relay)              |
+-----------------------------------------------------------------------+
```

- `v` = relay protocol version. v1 = `0x01`. Unknown version → datagram
  silently dropped.
- `session_token` = 16 bytes (128 bits) of cryptographic randomness, issued
  by `POST /v1/relay/allocate` (see `signaling-protocol.md`). Encoded in
  Crockford base32 for display (`01HG7P3MA1B2C3D4E5` = 16 bytes), but sent
  as raw bytes on the wire.
- `payload` = the bytes the peer's QUIC stack handed us. The relay never
  parses these.

Total header overhead: **17 bytes per datagram**. QUIC's typical MTU is
1200-1452 bytes, so overhead is ≈1.3%.

## Server-side allocation table

In-memory map maintained by the relay:

```go
type RelayAlloc struct {
    Token        [16]byte
    SessionID    string         // ties back to the signaling session
    PeerA        *net.UDPAddr   // first peer to send a datagram with this token
    PeerB        *net.UDPAddr   // second peer (may be nil until they speak up)
    BytesRelayed uint64         // cumulative wire bytes (header + payload, both directions)
    CreatedAt    time.Time
    LastActivity time.Time
}
```

## Demux + forward flow (the hot path)

For each inbound UDP datagram:

```
1. Read into a single reusable buffer (sync.Pool).
2. If len < 17: drop silently (malformed).
3. Read v (byte 0). If != 0x01: drop silently.
4. Read token (bytes 1..16). Look up RelayAlloc in map. If miss: drop silently
   (this is normal; could be late arrivals after a session ended).
5. Compare datagram source addr to PeerA / PeerB:
     a. If matches PeerA → forward to PeerB (if PeerB != nil)
     b. If matches PeerB → forward to PeerA
     c. If matches neither AND PeerB == nil → set PeerB = source addr,
        forward to PeerA. (This is the "second peer registers" step.)
     d. If matches neither AND PeerB != nil → drop silently (a third
        party is trying to inject; ignore).
6. Update BytesRelayed += len(datagram).
7. If BytesRelayed > FSEND_MAX_RELAY_BYTES_PER_SESSION:
     a. Send a small "kill" datagram back to both peers (type 0xFF in payload
        position; clients interpret as forced disconnect)
     b. Remove from map.
8. Update LastActivity = now.
```

Critical: **all of this is one hashmap read + one socket write per packet.**
No allocation in the common case (buffer pool), no parsing of the payload,
no per-packet logging. Target throughput: hardware-limited by the NIC, not
the relay logic.

## Idle eviction

A janitor goroutine runs every 30s:
- Iterate the allocation table.
- Evict any entry where `LastActivity > FSEND_SESSION_IDLE_TIMEOUT` ago.
- Evict any entry older than `--ttl-paired` from signaling (safety net).

## Why this design

**Why a 16-byte token instead of a smaller one?**
- 128 bits is the standard width for crypto-strong opaque tokens.
- 4 bytes (32 bits) would be brute-forceable on a busy relay.
- 8 bytes (64 bits) is borderline; 16 is comfortably safe.

**Why not use QUIC connection IDs for demux?**
- QUIC connection IDs are part of the QUIC header and visible to the relay,
  yes — but peers can rotate them (and quic-go does, by default). Tracking
  rotations means parsing QUIC headers, which means our "blind pipe" property
  is suddenly subject to QUIC version changes. The session-token approach is
  protocol-agnostic and survives any QUIC evolution.

**Why drop unknown-token datagrams silently instead of replying with an
error?**
- Standard practice for UDP services that don't want to amplify scans.
- Replies to bad packets create a DDoS amplification vector.
- Diagnostic mode (`FSEND_LOG_LEVEL=debug`) logs counters; never replies.

**Why count wire bytes, not payload bytes?**
- Wire bytes are what costs you the bandwidth bill.
- The 17-byte overhead is so small it doesn't meaningfully distort accounting.
- Simpler than tracking "the inner thing" we explicitly don't want to parse.

## Failure handling

| Scenario | Behavior |
|---|---|
| Token never seen (allocation expired or never issued) | Drop datagram silently |
| Allocation exists but only one peer has spoken | Forward when second peer's datagram arrives (set PeerB) |
| One peer goes silent mid-transfer | Other peer's datagrams keep being received but never forwarded (other addr is gone); session idle-evicted after timeout |
| Both peers go silent | Idle-evicted; no harm |
| Datagram size exceeds typical MTU | Forward as-is; UDP doesn't care, OS handles fragmentation if any. We do not split / reassemble. |
| Per-session byte cap reached | Send kill datagram to both peers, evict from table |
| Relay process restart | All allocations lost. Clients see no traffic, time out, fail over to a new code session (rare but acceptable) |

## Metrics surfaced (for self-hosters; future Prometheus endpoint)

- `fsend_relay_sessions_active` (gauge)
- `fsend_relay_bytes_total` (counter, by direction)
- `fsend_relay_packets_total` (counter)
- `fsend_relay_packets_dropped_total` (counter, by reason: bad_version, bad_token, third_party)
- `fsend_relay_sessions_evicted_total` (counter, by reason: idle, byte_cap, ttl)

Not in v1 surface (no Prometheus endpoint yet). Logged at `debug` level for
self-hosters who want to grep.

## What is NOT in v1

- **No congestion control / fair queuing.** All sessions share the OS UDP
  socket equally. A misbehaving session is bounded by the per-session byte
  cap, not by fair-queue scheduling.
- **No multi-region relay.** Single binary, one address per server. Global
  scale-out is a v2+ topology problem.
- **No relay-to-relay forwarding.** Both peers MUST be able to reach the same
  relay. (They can, via TCP/UDP 443 from anywhere on the public internet.)
- **No NACK / retry at the relay layer.** QUIC's loss recovery handles it
  end-to-end; the relay just shovels packets.
