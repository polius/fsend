# Running your own pairing server

> **Do you need this? Probably not.** The `fsend` CLI is just a local
> binary — there's nothing to host to *use* fsend, and it works out of the
> box. This page is only about the **pairing server**: an optional service
> that helps two peers find each other **across the internet**, and relays
> the transfer when a direct connection can't be made. On the same network
> it isn't involved at all — peers discover each other over the LAN (mDNS)
> and the bytes go straight across, even with the server offline.

By default, fsend uses a free pairing server at `fsend.alzina.dev`, run by
the maintainer on a best-effort basis (no uptime guarantee). It does two
jobs:

- **Pairing (always)** — both peers post a one-way slot derived from the
  share code, and the server tells each where the other is. It never
  learns the code or anything about your files.
- **Relay (fallback only)** — when NAT topology blocks a direct
  connection, it forwards the **encrypted** bytes between the two sides.
  It can't decrypt them, but they do pass through it.

Run your own when you want:

- **Control** — set your own rate limits, require an access password, tune
  any knob, instead of living with a shared instance's fixed policy.
- **Isolation** — an org-internal or air-gapped deployment, where even
  relayed (encrypted) traffic stays on infrastructure you control.
- **Reliability** — uptime you manage yourself, rather than a free server
  with no guarantee.

Clients point at it with `fsend --connect host:443`. The same `fsend`
binary is also the server, so there's nothing extra to build — this guide
deploys it with Docker Compose, behind Caddy for automatic TLS.

## Before you start

You'll need:

- A publicly reachable host (a public IP) with **Docker** and **Docker
  Compose v2**.
- A domain or subdomain you control, with an `A` record pointing at that
  host — e.g. `fs.example.com`.
- Three inbound ports open:
  - `80/tcp` — Let's Encrypt certificate issuance
  - `443/tcp` — HTTPS signaling
  - `443/udp` — UDP relay (file data)

## What you're deploying

Two containers on one host. Caddy terminates TLS on TCP/443 and proxies
HTTP signaling to fsend's `:8080`; UDP/443 goes straight to the fsend
container, so Caddy is never in the data path.

```
                ┌───────────────── Your VM ──────────────────┐
                │                                            │
  tcp/443  ────►│  ┌─────────┐                ┌───────────┐  │
  HTTPS sig.    │  │  Caddy  │ ──http:8080──► │  fsend    │  │
                │  │ (TLS)   │                │  server   │  │
  tcp/80   ────►│  │ + ACME  │                │           │  │
  cert renewal  │  └─────────┘                │           │  │
                │                             │ :443 udp  │  │
  udp/443  ─────────────── direct ───────────►│  (relay)  │  │
  QUIC data     │                             └───────────┘  │
                │                                            │
                └────────────────────────────────────────────┘
```

TCP/443 and UDP/443 share a number but are different protocols, so both
bind at once.

## Deploy

### 1. Download the compose stack

```sh
mkdir fsend-server && cd fsend-server
curl -fsSL https://raw.githubusercontent.com/polius/fsend/main/deploy/compose/docker-compose.yml -O
curl -fsSL https://raw.githubusercontent.com/polius/fsend/main/deploy/compose/Caddyfile -O
```

### 2. Start the stack

```sh
export FSEND_DOMAIN=fs.example.com   # ← your domain
docker compose up -d
```

On first start, Caddy requests a Let's Encrypt certificate and renews it
indefinitely after that — there's no manual cert handling, ever.

### 3. Verify

```sh
curl https://fs.example.com/v1/health
# {"status":"ok",…}
```

No OK response? See [Troubleshooting](#troubleshooting).

## Use your server from clients

Run this **once** on every client that should use your server instead of
the default:

```sh
fsend --connect fs.example.com:443
```

It's saved to the client's
[config file](usage.md#choosing-a-server---connect), so every later
`fsend` uses your server automatically — no need to repeat the flag.
Switch back to the public default any time with `fsend --connect default`.

## Require a password (optional)

By default, anyone who knows the address can use the server. To lock it
down, set `FSEND_SERVER_PASSWORD` in `docker-compose.yml` (it ships empty)
and restart:

```yaml
    environment:
      FSEND_SERVER_PASSWORD: "your-secret"
```

```sh
docker compose up -d
```

Now every endpoint except `/v1/health` requires the password, and clients
append it to `--connect`, comma-separated:

```sh
fsend --connect fs.example.com:443,your-secret
```

Connecting without it — or with the wrong one — fails with `E028`.

## Operations

- **Logs.** `docker compose logs -f fsend` for the server,
  `docker compose logs -f caddy` for TLS and the proxy. The default log
  level (`info`) records only lifecycle events — no per-transfer lines, no
  IPs, no share codes.
- **Update.** `docker compose pull && docker compose up -d`. The image
  tracks the floating `poliuscorp/fsend:latest` tag; pin a version tag if
  you want immutable, reproducible upgrades.
- **Backup.** Nothing to back up. Pairing state lives in RAM and evicts
  within an hour; certificates live in the `caddy_data` volume, and Caddy
  reissues them automatically if you lose it.
- **Tuning.** Every knob is an environment variable — see
  [Configuration](#configuration).

## Metrics

`GET /v1/metrics` returns a small JSON snapshot for monitoring:

```json
{
  "version": "1.8.0",
  "uptime_seconds": 84213,
  "sessions_active": 3,
  "sessions_created_total": 1284,
  "sessions_paired_total": 901,
  "sessions_rejected_total": { "rate_limit": 12, "concurrency_limit": 3, "unauthorized": 0 },
  "relay": {
    "forwarding": true,
    "healthy": true,
    "transfers_active": 1,
    "transfers_total": 412,
    "transfers_capped_total": 4,
    "bytes_forwarded_total": 5839201023,
    "peak_transfer_bytes": 940000000
  }
}
```

Every value is an **aggregate count or gauge** — there is no per-IP,
per-session, or per-code data, so it can't leak anything the server doesn't
store. That's deliberate: the endpoint is public on an open server precisely
so anyone can verify the server holds nothing sensitive.

| Field | What it tells you |
|---|---|
| `version` | Build currently running. |
| `uptime_seconds` | Seconds since the process started — spot restarts. |
| `sessions_active` | Pairing sessions live right now — current load. |
| `sessions_created_total` | Sessions a sender opened since boot — total usage. |
| `sessions_paired_total` | Sessions a receiver successfully joined. Compared to `sessions_created_total`, the gap is senders who never found a receiver (abandoned codes). |
| `sessions_rejected_total.rate_limit` | Sessions refused by the per-IP rate cap (`FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE`) — bursts or brute-force. |
| `sessions_rejected_total.concurrency_limit` | Sessions refused by the per-IP concurrency cap (`FSEND_SERVER_MAX_SESSIONS_PER_IP`). |
| `sessions_rejected_total.unauthorized` | Wrong/missing `FSEND_SERVER_PASSWORD` — brute-force or misconfigured clients (always `0` on an open server). |
| `relay.forwarding` | Whether the relay carries data (`false` in pairing + STUN-only mode). |
| `relay.healthy` | Relay read loop alive — mirrors `/v1/health`'s 503 signal. |
| `relay.transfers_active` | Relay transfers moving bytes right now. |
| `relay.transfers_total` | Relay transfers that actually forwarded data since boot. Divide by `sessions_paired_total` for your relay-fallback rate; the rest connected directly (P2P). A climbing ratio means hole-punching is failing more often. |
| `relay.transfers_capped_total` | Transfers cut off for hitting the per-session byte cap — raise the cap if this climbs. |
| `relay.bytes_forwarded_total` | Cumulative bytes relayed since boot — what the relay is costing you. |
| `relay.peak_transfer_bytes` | Largest single transfer so far; compare to your byte cap to see headroom. |
| `relay.budget_bytes_today` | Wire bytes forwarded so far in the current UTC day (both directions, same count as the budget). Approaching your configured `FSEND_RELAY_MAX_BYTES_PER_DAY` is your early warning; reaching it means the breaker has tripped and transfers are being refused until 00:00 UTC. |

The `relay` block is omitted when the server runs without a relay.

**Access.** `/v1/metrics` inherits the server's own openness, like every
endpoint except `/v1/health`:

- **Open server (no `FSEND_SERVER_PASSWORD`)** — public, no credentials:

  ```sh
  curl https://fs.example.com/v1/metrics
  ```

- **Password-protected server** — pass the password in the `X-Fsend-Auth`
  header (the same one clients use):

  ```sh
  curl -H "X-Fsend-Auth: your-secret" https://fs.example.com/v1/metrics
  ```

  Without it you get `401`. Note a **web browser can't open this URL**
  directly — it can't set the header — so use `curl`, or configure your
  monitoring scraper to send `X-Fsend-Auth` (Prometheus: a static header
  under the scrape job).

So a public server is transparently inspectable by anyone, and a locked-down
one keeps its metrics to whoever holds the password.

## Troubleshooting

| Symptom | Likely cause |
|---|---|
| `docker compose up` errors out asking for `FSEND_DOMAIN` | You forgot `export FSEND_DOMAIN=…`. Set it and rerun. |
| Caddy logs show "obtain certificate failed" (ACME error) | DNS A-record not yet pointing at this host, or `80/tcp` blocked at the firewall. |
| `curl` to `/v1/health` works but clients hit `E001` on cross-network transfers | `443/udp` blocked at the firewall — many setups open TCP only and forget UDP. |
| Pairing starts but the transfer hangs or drops | A reverse proxy in front of fsend (not the bundled Caddy) is buffering long-poll signaling. Caddy is configured for 30s read/write; nginx/Traefik need the same. |
| Clients hit `E028` ("pairing server requires a password") | You set `FSEND_SERVER_PASSWORD` on the server. Clients must connect with `fsend --connect host:443,<password>`. |

## Configuration

All optional; defaults shown. Set them under the `fsend` service's
`environment:` block in `docker-compose.yml`.

On startup the server prints its effective configuration, marking with a
`*` every setting you changed from its default — a quick way to confirm
your overrides were picked up (the password value is never printed, only
whether one is set):

```
fsend server configuration (2 of 9 customized; * = changed from default):
    FSEND_LOG_LEVEL                              info
  * FSEND_SERVER_PASSWORD                        (set)
    FSEND_SERVER_ADDR                            :8080
  * FSEND_SERVER_MAX_SESSIONS_PER_IP             10
    FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE  0 (unlimited)
    FSEND_RELAY_ENABLED                          true
    FSEND_RELAY_ADDR                             :443
    FSEND_RELAY_MAX_BYTES_PER_SESSION            0 (unlimited)
    FSEND_RELAY_MAX_BYTES_PER_DAY                0 (unlimited)
```

Variables fall into three groups: **server-wide** controls (logging and
auth, which apply to every endpoint), the **pairing / control plane** (its
TCP listener and per-IP session limits), and the **relay / data plane**
(the UDP listener — which also answers STUN — and its byte limits).

| Variable | Default | Notes |
|---|---|---|
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FSEND_SERVER_PASSWORD` | _(unset)_ | Shared secret restricting all endpoints except `/v1/health` — see [Require a password](#require-a-password-optional). |
| `FSEND_SERVER_ADDR` | `:8080` | TCP signaling listener. |
| `FSEND_SERVER_MAX_SESSIONS_PER_IP` | `0` (unlimited) | **Concurrency cap** — how many sessions one source IP may have **alive at once**. A session gates relay allocation too, so this caps relay access as well. Defaults to unlimited; **set a positive value to enable this DoS protection** (recommended on a public server). |
| `FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE` | `0` (unlimited) | **Rate cap** — how many **new** sessions one source IP may **create per minute**. Distinct from the concurrency cap above: this limits inflow over time, that limits standing count. Defaults to unlimited; **set a positive value to enable this DoS protection** (recommended on a public server). |
| `FSEND_RELAY_ENABLED` | `true` | Set `false` for **pairing + STUN only**: the server still helps peers hole-punch a direct path, but carries no file data. See [Pairing-only mode](#pairing-only-mode-no-relay). |
| `FSEND_RELAY_ADDR` | `:443` | UDP relay listener — also the STUN endpoint, so it stays in use even when forwarding is off. The server tells clients to dial `<request-host>:<this port>`, so no separate public-address knob is needed. |
| `FSEND_RELAY_MAX_BYTES_PER_SESSION` | `0` (unlimited) | Per-session relay cap — wire bytes after compression. Defaults to unlimited; set a value to bound per-transfer bandwidth. Accepts `B`, `KB`, `MB`, `GB`, `TB` suffixes (decimal, e.g. `500MB`, `1GB`) or a plain byte count (`1000000000`). |
| `FSEND_RELAY_MAX_BYTES_PER_DAY` | `0` (unlimited) | **Egress budget** — wire bytes the relay forwards **per UTC day** across all sessions. The Denial-of-Wallet ceiling: once spent, the relay stops forwarding and refuses new transfers until 00:00 UTC. Bounds a distributed abuser that the per-IP caps can't. Same units as above. Defaults to unlimited; **set a value to bound your bandwidth bill**. **What's counted:** every datagram the relay forwards, once, in **both directions** (payload one way, ACKs/control the other) — i.e. outbound bytes, matching how egress is billed. Not counted: STUN (separate path), and same-network/direct transfers (never touch the relay). Note the relay physically receives *and* sends each byte, so if your host also bills **inbound** traffic, real cost ≈ 2× the budget. |

### Pairing-only mode (no relay)

Set `FSEND_RELAY_ENABLED=false` to run the server as a pure matchmaker: it
answers STUN (so peers can still hole-punch directly across NATs) but never
forwards a single byte of file data. Use this when you want to help peers
find each other without paying for — or being liable for — relayed traffic.

The trade-off: when hole-punching fails (typically symmetric NAT on both
ends), there is no fallback. Those transfers fail with a clear
"relay forwarding disabled" error instead of relaying. Same-LAN and
direct-internet transfers are unaffected. The UDP listener still binds (STUN
needs it); only forwarding is off.
