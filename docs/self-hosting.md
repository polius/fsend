# Running your own pairing server

> **Do you need this?** Probably not. The `fsend` CLI is a local binary —
> there's nothing to host to *use* fsend, and it already works out of the
> box. This page is only about the **pairing server**: an optional,
> self-hostable service that two peers use to find each other **across the
> internet** — and that relays the transfer when a direct connection can't
> be established. On the same local network it isn't used at all: peers
> discover each other over the LAN (via mDNS) and the bytes go straight
> across, even if the server is offline.

By default, fsend uses a free pairing server at `fsend.alzina.dev`,
operated by the maintainer on a best-effort basis (no uptime guarantee).
The server plays two roles:

- **Pairing** *(always)* — both peers post a one-way slot derived from
  the share code; the server tells each where the other is. It never
  learns the share code or anything about your files.
- **Relay** *(fallback only)* — when NAT topology blocks a direct
  peer-to-peer connection, the server forwards the **encrypted** bytes
  between the two sides. It can't decrypt them, but they do pass through
  it.

You'd run your own when you want:

- **Control** — set your own rate limits, require an access password,
  tune any knob — instead of living with a shared instance's fixed policy.
- **Isolation** — an org-internal or air-gapped deployment where even
  relayed (encrypted) traffic never leaves infrastructure you control.
- **Reliability** — uptime you manage yourself, rather than a free server
  with no guarantee.

Clients then point at it with `fsend --connect host:443`. The same
`fsend` binary doubles as the server, so there's nothing extra to build —
this guide deploys it via Docker compose, behind Caddy for automatic TLS.

## Before you start

You need:

- A publicly reachable host (a public IP) with **Docker** and **Docker
  compose v2**
- A domain (or subdomain) you control, with an `A` record pointing it at
  that host — e.g. `fs.example.com`
- These inbound ports open:
  - `80/tcp` — Let's Encrypt cert issuance
  - `443/tcp` — HTTPS signaling
  - `443/udp` — UDP relay (file data)

## What you're deploying

Two containers on one host. Caddy terminates TLS on TCP/443 and proxies
HTTP signaling to fsend's `:8080`. UDP/443 flows directly to the fsend
container — Caddy isn't in the data path.

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

TCP/443 and UDP/443 share the port number but are different protocols,
so both can bind simultaneously.

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

Caddy requests a Let's Encrypt cert on first start and renews it
indefinitely thereafter — there is no manual cert handling at any point.

### 3. Verify

```sh
curl https://fs.example.com/v1/health
# {"status":"ok",…}
```

If you don't get an OK response, see [Troubleshooting](#troubleshooting).

## Use your server from clients

Run this **once** on every client that should use your server instead of
the default:

```sh
fsend --connect fs.example.com:443
```

It's saved to the client's config, so every later `fsend` uses your
server automatically — no need to repeat the flag. The file lives at
`~/.config/fsend/config.json` (Linux),
`~/Library/Application Support/fsend/config.json` (macOS), or
`%LOCALAPPDATA%\fsend\config.json` (Windows). Revert to the public default
with `fsend --connect default`.

## Require a password (optional)

By default anyone who knows the address can use the server. To restrict
it, set `FSEND_SERVER_PASSWORD` in `docker-compose.yml` (it ships empty)
and restart:

```yaml
    environment:
      FSEND_SERVER_PASSWORD: "your-secret"
```

```sh
docker compose up -d
```

Every endpoint except `/v1/health` now requires the password. Clients
append it to `--connect`, comma-separated:

```sh
fsend --connect fs.example.com:443,your-secret
```

Connecting without it (or with the wrong one) fails with `E028`.

## Operations

- **Logs.** `docker compose logs -f fsend` for the server,
  `docker compose logs -f caddy` for TLS and the reverse proxy. Default
  log level is `info` — lifecycle events only. No per-transfer lines,
  no IPs, no share codes.
- **Update.** `docker compose pull && docker compose up -d`. The image
  defaults to the floating `poliuscorp/fsend:latest` tag; pin a specific
  version tag if you want immutable, reproducible upgrades.
- **Backup.** Nothing to back up. Pairing state lives in RAM and evicts
  within an hour. Cert state lives in the `caddy_data` Docker volume —
  Caddy reissues automatically if you lose it.
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

| Variable | Default | Notes |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | TCP signaling listener. |
| `FSEND_UDP_ADDR` | `:443` | UDP relay listener. The server tells clients to dial `<request-host>:<this port>`, so no separate public-address knob is needed. |
| `FSEND_SERVER_PASSWORD` | _(unset)_ | Shared secret restricting all endpoints except `/v1/health` — see [Require a password](#require-a-password-optional). |
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FSEND_MAX_SESSIONS_PER_IP` | `5` | Concurrent sessions per client IP. |
| `FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN` | `30` | New-session rate limit. |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `1GB` | Per-session relay cap — wire bytes after compression. Tune to your bandwidth budget. Accepts `B`, `KB`, `MB`, `GB`, `TB` suffixes (decimal, e.g. `500MB`, `1GB`) or a plain byte count (`1073741824`). |
