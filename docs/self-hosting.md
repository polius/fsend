# Self-hosting

The same `fsend` binary doubles as the pairing server. This guide deploys
it via Docker compose, behind Caddy for automatic TLS — an alternative to
the default `fsend.alzina.dev`, which is best-effort and not guaranteed.

## Before you start

You need:

- A VM with a public IP
- A domain (or subdomain) you control, e.g. `fs.example.com`
- An `A` record pointing that domain at the VM's public IP
- **Docker** and **Docker compose v2** on the VM
- These inbound ports open on the VM:
  - `80/tcp` — Let's Encrypt cert issuance
  - `443/tcp` — HTTPS signaling
  - `443/udp` — UDP relay (file data)

## What you're deploying

Two containers on one VM. Caddy terminates TLS on TCP/443 and proxies
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

### 1. Point your domain at the VM

Create an `A` record from `fs.example.com` → your VM's public IP, and
wait for it to propagate (usually under a minute). Caddy needs this to
complete the Let's Encrypt HTTP-01 challenge in step 3.

Verify before going further:

```sh
dig +short fs.example.com    # should print the VM's public IP
```

### 2. Download the compose stack

```sh
mkdir fsend-server && cd fsend-server
curl -fsSL https://raw.githubusercontent.com/polius/fsend/main/deploy/compose/docker-compose.yml -O
curl -fsSL https://raw.githubusercontent.com/polius/fsend/main/deploy/compose/Caddyfile -O
```

### 3. Start the stack

```sh
export FSEND_DOMAIN=fs.example.com
docker compose up -d
```

Caddy requests a Let's Encrypt cert on first start and renews it
indefinitely thereafter — there is no manual cert handling at any point.

### 4. Verify

```sh
curl https://fs.example.com/v1/health
# {"status":"ok",…}
```

If you don't get an OK response, see [Troubleshooting](#troubleshooting).

## Use your server from clients

On every client that should use your server instead of the default:

```sh
fsend --connect fs.example.com:443
```

Persists to `~/.config/fsend/config.json` (Linux),
`~/Library/Application Support/fsend/config.json` (macOS), or
`%APPDATA%\fsend\config.json` (Windows). Revert to the public default
with `fsend --connect default`.

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
| Caddy logs show "obtain certificate failed" (ACME error) | DNS A-record not yet pointing at this VM, or `80/tcp` blocked at the firewall. |
| `curl` to `/v1/health` works but clients hit `E001` on cross-network transfers | `443/udp` blocked at the firewall — many setups open TCP only and forget UDP. |
| Pairing starts but the transfer hangs or drops | A reverse proxy in front of fsend (not the bundled Caddy) is buffering long-poll signaling. Caddy is configured for 30s read/write; nginx/Traefik need the same. |
| Clients hit `E028` ("pairing server requires a password") | You set `FSEND_SERVER_PASSWORD` on the server. Clients must connect with `fsend --connect host:443,<password>`. |

## LAN-only / dev mode

To skip TLS entirely for local testing on a trusted network:

```sh
docker run -p 443:443/udp -p 8080:8080/tcp poliuscorp/fsend server
```

Clients connect with `fsend --connect host:8080` — the client picks
HTTP automatically for IPs and `localhost`, HTTPS otherwise. **Do not**
expose this to the public internet — signaling carries share codes and
bearer tokens in cleartext.

## Configuration

All optional; defaults shown. Set under the `environment:` block in
`docker-compose.yml`.

| Variable | Default | Notes |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | TCP signaling listener. |
| `FSEND_UDP_ADDR` | `:443` | UDP relay listener. The server tells clients to dial `<request-host>:<this port>`, so no separate public-address knob is needed. |
| `FSEND_SERVER_PASSWORD` | _(unset)_ | Shared secret. When set, every endpoint except `/v1/health` requires it. Clients pass it via `fsend --connect <host>,<password>`. |
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FSEND_MAX_SESSIONS_PER_IP` | `5` | Concurrent sessions per client IP. |
| `FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN` | `30` | New-session rate limit. |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `100MiB` | Accepts `500m`, `1GiB`, `104857600`. |
