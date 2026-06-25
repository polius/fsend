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

Variables are grouped by subsystem: **general**, **pairing** (the TCP
signaling/control plane), and **relay** (the UDP data plane, which also
answers STUN).

| Variable | Default | Notes |
|---|---|---|
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FSEND_SERVER_PASSWORD` | _(unset)_ | Shared secret restricting all endpoints except `/v1/health` — see [Require a password](#require-a-password-optional). |
| `FSEND_PAIRING_ADDR` | `:8080` | TCP signaling listener. |
| `FSEND_PAIRING_MAX_SESSIONS_PER_IP` | `5` | Concurrent sessions per client IP. |
| `FSEND_PAIRING_MAX_NEW_SESSIONS_PER_IP_PER_MIN` | `30` | New-session rate limit, per source IP. |
| `FSEND_RELAY_ENABLED` | `true` | Set `false` for **pairing + STUN only**: the server still helps peers hole-punch a direct path, but carries no file data. See [Pairing-only mode](#pairing-only-mode-no-relay). |
| `FSEND_RELAY_ADDR` | `:443` | UDP relay listener — also the STUN endpoint, so it stays in use even when forwarding is off. The server tells clients to dial `<request-host>:<this port>`, so no separate public-address knob is needed. |
| `FSEND_RELAY_MAX_BYTES_PER_SESSION` | `1GB` | Per-session relay cap — wire bytes after compression. Tune to your bandwidth budget. Accepts `B`, `KB`, `MB`, `GB`, `TB` suffixes (decimal, e.g. `500MB`, `1GB`) or a plain byte count (`1000000000`). |

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
