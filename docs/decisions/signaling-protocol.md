# Decision: Signaling protocol (client ↔ server)

**Status:** Locked
**Date:** 2026-06-03
**Scope:** HTTP-level interaction between the CLI clients and `fsend-server`
for code matching, address reflection, and ICE candidate exchange. Does NOT
cover the relay-fallback UDP path — see `relay-protocol.md`.

---

## Design principles

1. **Plain HTTP/1.1 (no WebSocket, no SSE).** Long-polling with reasonable
   timeouts is enough for our use case (short-lived sessions, low message
   volume) and dramatically simplifies the server. Reverse proxies handle
   long-polling cleanly without WebSocket gymnastics.
2. **Stateless server where possible.** Session state is small and lives in
   memory; rebuild on restart is acceptable for v1. No DB.
3. **Compact JSON.** Volume is tiny (a few KB per session), so JSON's
   debuggability is worth the bytes. Use `application/json`.
4. **Idempotent endpoints where it doesn't cost us.** Retries are safer.
5. **No client-side auth.** Code phrase is the only credential; verified
   indirectly via PAKE at the peer-to-peer layer.

## Transport

- Scheme: HTTPS (terminated by operator's reverse proxy).
- Base URL: `https://{server-host}/`
- HTTP/1.1 acceptable; HTTP/2 nice-to-have (reverse proxy decides).
- All endpoints under `/v1/...` for versioning.
- All POST bodies are JSON. All responses are JSON with a top-level object.

## Session lifecycle

```
   SENDER                                     SERVER                                     RECEIVER
   ──────                                     ──────                                     ────────
   POST /v1/session ──────────────────────►
                                              creates session{code, sender_addr,
                                              waiting_for=receiver},
                                              starts 60s TTL
   ◄──── 200 { session_id, code, ... }

   POST /v1/session/<code>/wait ──────────►  (long-poll, 25s max per request)
                                              ...waits...

                                                                       ◄────────── POST /v1/session/<code>/join
                                              pairs receiver_addr,
                                              hands sender's pub addr,
                                              releases sender's long-poll
                                              ──────────► 200 { peer_addr, ice_creds, ... }
                                              ◄────────── 200 { peer_addr, ice_creds, ... }
   ◄──── 200 { peer_addr, ice_creds, ... }

   (both clients now have each other's reflected address; ICE proceeds out-of-band)

   POST /v1/session/<id>/candidates  ────►   relays trickle ICE candidates
                                              ◄──── POST /v1/session/<id>/candidates
   ◄──── GET /v1/session/<id>/candidates
                                                                       ◄────── GET /v1/session/<id>/candidates

   (when ICE completes, peers send DELETE to clean up)
   DELETE /v1/session/<id> ───────────────►   removes session
```

## Endpoints

### `POST /v1/session` — sender creates a session

Request:
```json
{
  "client_version": "0.1.0"
}
```

Response (200):
```json
{
  "session_id": "01HG7P3M9XKN...",          // ULID, server-issued, opaque to clients
  "code": "abc-defg-jkm",                  // server-generated short code
  "your_observed_addr": "84.27.123.45:51022", // STUN-style reflection
  "ice_credentials": {
    "ufrag": "9aT4",                         // ICE username fragment
    "pwd": "rN8sX..."                        // ICE password
  },
  "ttl_seconds": 60,                         // how long until the unpaired session expires
  "server_version": "0.1.0"
}
```

Errors:
- `429 Too Many Requests` — per-IP rate limit hit
- `503 Service Unavailable` — server at session cap

### `POST /v1/session/<code>/join` — receiver joins by code

Request:
```json
{
  "client_version": "0.1.0"
}
```

Response (200):
```json
{
  "session_id": "01HG7P3M9XKN...",          // same session as the sender
  "your_observed_addr": "178.91.55.10:38291",
  "peer_observed_addr": "84.27.123.45:51022", // sender's reflected addr
  "peer_ice_credentials": {
    "ufrag": "9aT4",
    "pwd": "rN8sX..."
  },
  "your_ice_credentials": {
    "ufrag": "K2pM",
    "pwd": "wQ7t..."
  }
}
```

Errors:
- `404 Not Found` — no session with that code (typo, expired, or already paired)
- `409 Conflict` — session already has a receiver paired
- `429`, `503` — same as above

### `POST /v1/session/<code>/wait` — sender long-polls until paired

Request:
```json
{ "since": "2026-06-03T10:15:30Z" }  // optional, for resuming polls
```

Response (200, once paired):
```json
{
  "peer_observed_addr": "178.91.55.10:38291",
  "peer_ice_credentials": { "ufrag": "K2pM", "pwd": "wQ7t..." }
}
```

Response (204 No Content): timeout reached (25s default) without pairing —
client immediately retries until the session TTL expires or the user aborts.

### `POST /v1/session/<id>/candidates` — push trickle ICE candidates

Request:
```json
{
  "candidates": [
    "candidate:1 1 udp 2122260223 192.168.1.5 51022 typ host",
    "candidate:2 1 udp 1686052607 84.27.123.45 51022 typ srflx raddr 192.168.1.5 rport 51022"
  ]
}
```

Response (204): accepted.

### `GET /v1/session/<id>/candidates?since=<idx>` — pull peer's ICE candidates

Long-poll, 25s max. Returns whatever new candidates the peer has pushed since
the `since` index.

Response (200):
```json
{
  "candidates": [ "candidate:..." ],
  "next_since": 7,
  "ended": false                  // true → peer signaled candidate gathering complete
}
```

### `DELETE /v1/session/<id>` — clean up after successful pairing

Response (204).

The server will GC stale sessions on its own (TTL), but clients should clean
up promptly to free state.

### `POST /v1/relay/allocate` — request a relay allocation (fallback path)

See `relay-protocol.md` for the relay-side wire details. Signaling-wise, the
client POSTs here when ICE has failed:

Request:
```json
{ "session_id": "01HG7P3M9XKN..." }
```

Response (200):
```json
{
  "relay_addr": "fs.alzina.dev:443",      // UDP endpoint to send to
  "session_token": "01HG7P3MA1B2C3D4E5",  // opaque token; prepend to each UDP datagram
  "ttl_seconds": 600
}
```

### `GET /v1/health` — liveness check

Response (200):
```json
{ "status": "ok", "version": "0.1.0", "uptime_seconds": 12345 }
```

## Server state model

In-memory map (no DB in v1):

```go
type Session struct {
    ID               string        // ULID
    Code             string        // xxx-xxxx-xxx, unique while session alive
    SenderAddr       string        // remote addr seen at /v1/session
    SenderICE        IceCreds
    ReceiverAddr     string        // remote addr seen at /v1/session/<code>/join; empty until paired
    ReceiverICE      IceCreds
    State            string        // "waiting" | "paired" | "complete"
    CreatedAt        time.Time
    PairedAt         time.Time
    SenderCandidates []string      // trickle queue from sender
    ReceiverCandidates []string    // trickle queue from receiver
    SenderWaiters    chan struct{} // wakes long-polls when receiver joins
}
```

Server runs a janitor goroutine every 10s that evicts:
- Sessions in `waiting` state older than `--ttl-unpaired` (default 60s)
- Sessions in `paired` state older than `--ttl-paired` (default 600s — ICE
  should complete in seconds, this is a safety net)
- Sessions in `complete` state immediately

## Code generation (server-side)

Server generates codes server-side rather than letting clients propose them.
Reasons:
- Avoids collision-handling logic on the client
- Lets the server enforce code-space exhaustion limits (back-pressure)
- Codes are short enough that server-side generation is trivial

Algorithm: `crypto/rand` over `[a-hjkmnp-z]` (23-letter alphabet), 10 chars,
formatted as `xxx-xxxx-xxx`. On collision with an active session, regenerate
(birthday-bound is huge: 23^10 ≈ 4×10^13).

## Rate limits (enforced by server, see PROJECT_SPEC.md env vars)

- New-session attempts per IP per minute → `FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN`
- Concurrent sessions per IP → `FSEND_MAX_SESSIONS_PER_IP`
- Per-IP token bucket on `/v1/session/*/candidates` POST (anti-flood for ICE
  candidate spam): 100 req/min default, hardcoded (not env-var-tunable in v1)

Rate-limited responses use HTTP 429 with `Retry-After` header.

## Timeouts

| Operation | Default | Override |
|---|---|---|
| Unpaired session TTL | 60s | (env var, future) |
| Paired session TTL | 600s | (env var, future) |
| Long-poll max duration | 25s | hardcoded (works under most reverse proxy defaults) |
| Client HTTP request timeout | 30s | hardcoded |
| Client retry policy | exponential backoff, max 3 attempts | hardcoded |

## What is NOT in v1 signaling

- **No authentication of clients.** The code phrase is the only credential
  and is verified peer-to-peer via PAKE. The server is intentionally trustless
  beyond rate limits.
- **No persistence.** Server restart drops all sessions; clients see 404 and
  the user retries with a fresh code. Acceptable for v1 — sessions are <60s.
- **No WebRTC SDP exchange.** ICE candidates are exchanged as raw RFC 5245
  candidate strings; the rest of SDP is unnecessary because both peers
  already know the channel binding (PAKE key), the transport (QUIC), and the
  application (file transfer).
- **No relay routing through the signaling channel.** Once relay is needed,
  clients switch to the dedicated UDP path (see `relay-protocol.md`).
