# Run your own pairing server

**You probably don't need this.** fsend works out of the box with the
free public server at `fsend.alzina.dev` (best effort, no uptime
guarantee). On the same network, fsend doesn't need a server at all.

The server does two jobs:

- **Pairing.** It helps two devices find each other across the internet.
  It never learns the code or anything about your files.
- **Relay.** When no direct path exists, it forwards the encrypted data.
  It can't read it.

Run your own for your own limits, a password, uptime you control, or to
keep relayed traffic on your own machines. The `fsend` binary is also the
server. This guide runs it with Docker Compose, behind Caddy for HTTPS.

## Before you start

You need:

- A server with a public IP address.
- Docker with Compose v2.
- A domain name with an `A` record pointing at the server, for example
  `fs.example.com`.

### Ports to open

Open these three inbound ports in your firewall and in your cloud
provider's security group:

| Port | Protocol | What it's for |
|---|---|---|
| **80** | TCP | Getting the HTTPS certificate (Let's Encrypt) |
| **443** | TCP | Pairing, and the relay for networks that block UDP |
| **443** | UDP | The relay, and finding direct paths (STUN) |

Port 443 is needed for **both TCP and UDP**. Opening only TCP is the
most common mistake: transfers then work, but they're slower and
connect directly less often.

## Set it up

**1. Download the two files:**

```sh
mkdir fsend-server && cd fsend-server
curl -fsSLO https://raw.githubusercontent.com/polius/fsend/main/deploy/compose/docker-compose.yml
curl -fsSLO https://raw.githubusercontent.com/polius/fsend/main/deploy/compose/Caddyfile
```

**2. Start it with your domain:**

```sh
export FSEND_DOMAIN=fs.example.com
docker compose up -d
```

Caddy gets the HTTPS certificate on the first start and renews it on its
own.

**3. Check it:**

```sh
curl https://fs.example.com/health
# {"status":"ok"}
```

No `ok`? See [Troubleshooting](#troubleshooting).

## Use it from your devices

Run this once on each device:

```sh
fsend --connect fs.example.com:443
```

Every later `fsend` uses your server. To check it end to end, run
`fsend doctor`. To go back to the public server, run
`fsend --connect default`.

## Require a password (optional)

By default, anyone who knows the address can use your server. To limit
it, set a password in `docker-compose.yml`:

```yaml
    environment:
      FSEND_SERVER_PASSWORD: "your-secret"
```

Then run `docker compose up -d` again. Use a long random value, such as
the output of `openssl rand -base64 24`. Wrong guesses aren't rate
limited, so length is what protects it.

Devices then connect with the password after a comma:

```sh
fsend --connect fs.example.com:443,your-secret
```

Without it, they get `E028`. `/health` stays open, so monitoring needs
no password.

## Day to day

- **Update:** `docker compose pull && docker compose up -d`. To pin a
  version, set `FSEND_VERSION=1.17.0` before running it. Waiting codes
  survive the restart.
- **Logs:** `docker compose logs -f fsend`. By default the server logs no
  IP addresses or codes. `FSEND_LOG_LEVEL=debug` adds IP addresses.
- **Backups:** nothing to back up. Keep the `caddy_data` volume, so
  Caddy doesn't have to request a new certificate.
- **One server per domain.** Don't run two copies behind a load
  balancer: they can't see each other's codes. Use a bigger machine
  instead.

## Troubleshooting

| Problem | Fix |
|---|---|
| `docker compose up` asks for `FSEND_DOMAIN` | Run `export FSEND_DOMAIN=…` first. |
| Caddy logs "obtain certificate failed" | The `A` record doesn't point at the server yet, or port 80/TCP is closed. |
| `/health` works, but `fsend doctor` fails the UDP check, or transfers between networks are slow or fail with `E042` | Port 443/UDP is closed. |
| Devices get `E028` | The server has a password. Connect with `fsend --connect host:443,<password>`. |
| Old clients get `E060` | They're too old for this server. Run `fsend --update` on them. |
| Behind your own nginx or Traefik, transfers hang | The proxy must allow WebSocket upgrades on `/v1/relay/ws`, with timeouts over 30 s. |

## Configuration

Every setting is optional. Set them under `environment:` in
`docker-compose.yml`, then run `docker compose up -d`. At startup the
server prints its settings and marks the ones you changed with `*`.

**Limits** (recommended on a public server; the compose file has them
commented out):

| Variable | Default | What it does |
|---|---|---|
| `FSEND_SERVER_MAX_SESSIONS_PER_IP` | unlimited | Codes one IP address can have open at once. |
| `FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE` | unlimited | New codes one IP address can open per minute. |
| `FSEND_RELAY_MAX_BYTES_PER_SESSION` | unlimited | Most data one relayed transfer may use, e.g. `50GB`. |
| `FSEND_RELAY_MAX_BYTES_PER_DAY` | unlimited | Most data the relay forwards per UTC day, e.g. `1TB`. Caps your bandwidth bill. |
| `FSEND_MIN_CLIENT_VERSION` | none | Refuses older fsend versions, e.g. `1.15.0`. They get `E060`. |

**Access and logging:**

| Variable | Default | What it does |
|---|---|---|
| `FSEND_SERVER_PASSWORD` | none | Requires a password. See [above](#require-a-password-optional). |
| `FSEND_SERVER_PASSWORD_FILE` | none | Reads the password from a file instead (Docker secrets). |
| `FSEND_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. |
| `FSEND_LOG_FORMAT` | `text` | `json` for log collectors. |
| `FSEND_METRICS_ADDR` | off | Serves metrics on a separate port. See [Metrics](#metrics). |

**Network** (the compose file already sets what it needs):

| Variable | Default | What it does |
|---|---|---|
| `FSEND_RELAY_ENABLED` | `true` | `false` turns the relay off. The server still pairs devices and helps them connect directly, but transfers that need a relay fail with `E014`. |
| `FSEND_SERVER_ADDR` | `:8080` | Where the server listens for pairing (behind Caddy). |
| `FSEND_RELAY_ADDR` | `:443` | Where the relay listens (UDP). |
| `FSEND_RELAY_PUBLIC_ADDR` | none | The relay address to give devices, as `host:port`. Set it when a CDN that can't carry UDP (such as Cloudflare's proxy) sits in front of the server. |
| `FSEND_SERVER_TRUSTED_PROXIES` | none (compose: private ranges) | Proxies whose `X-Forwarded-For` header is trusted, so the per-IP limits see the real client. |

## Metrics

Metrics are off by default. Set `FSEND_METRICS_ADDR: ":9090"` to serve
them on a separate port, reachable from the compose network:

- `/metrics`: Prometheus format (names start with `fsend_`).
- `/metrics.json`: the same, as JSON.

They're totals only, with nothing per user or per code. That port has no
password, so **never publish it**.

The most useful ones:

| Metric | What it tells you |
|---|---|
| `sessions_active` | Codes open right now. |
| `sessions_created_total` | Codes opened since the server started. |
| `sessions_paired_total` | Codes a receiver joined through the server. |
| `sessions_rejected_total` | Codes refused, by reason (`rate_limit`, `concurrency_limit`, `unauthorized`). |
| `relay.transfers_total` | Transfers that needed the relay. |
| `relay.bytes_forwarded_total` | Data the relay has forwarded. |
| `relay.budget_bytes_today` | Data forwarded today, against `FSEND_RELAY_MAX_BYTES_PER_DAY`. |

## IPv6

Add an `AAAA` record only if Docker has IPv6 turned on (`"ipv6": true`
and `"ip6tables": true` in `daemon.json`, and `enable_ipv6: true` on the
compose network). Otherwise IPv6 clients reach the server through
Docker's IPv4 proxy, and the server sees the wrong address for them.
