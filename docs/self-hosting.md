# Self-hosting

The default pairing server at `fsend.alzina.dev` is best-effort. To run
your own, the same `fsend` binary doubles as the server:

```sh
fsend server                 # config via env vars (FSEND_*)
fsend server --health-check  # probe /v1/health (exit 0 if healthy)
fsend server --help          # show all options
```

## Docker compose

The fastest production-quality setup uses the bundled compose stack
([`deploy/compose/`](../deploy/compose/)), which fronts the signaling
API with Caddy and provisions a Let's Encrypt cert automatically:

```sh
export FSEND_DOMAIN=fs.example.com
docker compose -f deploy/compose/docker-compose.yml up -d
fsend --connect fs.example.com:443    # on each client
```

For a quick LAN-only test (no TLS), use the image directly:

```sh
docker run -p 443:443/udp -p 8080:8080/tcp poliuscorp/fsend
```

## Ports

`fsend server` has two listeners: a **TCP HTTP signaling API**
(`FSEND_HTTP_ADDR`, default `:8080`) and a **UDP relay**
(`FSEND_UDP_ADDR`, default `:443`). The UDP relay only carries opaque
QUIC ciphertext between peers — TLS terminates at the peers, not at the
server — so it is the same listener whether or not you put HTTPS in
front of signaling.

### HTTP-only mode

No reverse proxy — fine for LAN, dev, or trusted networks. **Not**
recommended on the public internet.

| Port             | Direction | Purpose                                         |
|------------------|-----------|-------------------------------------------------|
| `8080/tcp`       | inbound   | Signaling HTTP API (clients POST session/join)  |
| `443/udp`        | inbound   | ICE STUN-style reflection + relay fallback (opaque QUIC datagrams) |

Clients connect with `fsend --connect http://host:8080`. If you change
`FSEND_UDP_ADDR`, also set `FSEND_PUBLIC_ADDR=host:port` to the address
clients should dial for relay.

### HTTPS mode

Reverse proxy with your own domain — recommended for any
public-internet deployment. Matches the `deploy/compose/` stack.

| Port             | Direction | Purpose                                              |
|------------------|-----------|------------------------------------------------------|
| `443/tcp`        | inbound   | HTTPS signaling — terminated by Caddy/nginx/Traefik  |
| `443/udp`        | inbound   | ICE STUN-style reflection + relay fallback — goes **directly** to the fsend container |
| `80/tcp`         | inbound   | Let's Encrypt ACME HTTP-01 challenge (cert issue/renew) |
| `8080/tcp`       | internal  | fsend signaling — only reachable by the proxy |

Clients then connect with `fsend --connect fs.example.com:443` (HTTPS
is the default scheme for non-local hosts). TCP/443 and UDP/443 share
the same port number but are different protocols, so both can bind
simultaneously.

No outbound ports beyond what your OS / Docker needs. The server makes
no outbound connections to clients.

## Configuration

All optional; defaults shown.

| Variable | Default | Notes |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | TCP signaling listener. |
| `FSEND_UDP_ADDR` | `:443` | UDP relay listener. |
| `FSEND_PUBLIC_ADDR` | = `FSEND_UDP_ADDR` | `host:port` clients dial for relay. Set when public ≠ bind (NAT, dev box). |
| `FSEND_SERVER_PASSWORD` | _(unset)_ | Shared secret. When set, every endpoint except `/v1/health` requires it. Clients pass it via `fsend --connect <host> <password>`. |
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FSEND_MAX_SESSIONS_PER_IP` | `5` | Concurrent sessions per client IP. |
| `FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN` | `30` | New-session rate limit. |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `100MiB` | Accepts `500m`, `1GiB`, `104857600`. |
| `FSEND_SESSION_IDLE_TIMEOUT` | `60s` | Go duration. |

Internet-exposed deployments need a TLS-terminating reverse proxy in
front of `:8080` — file data on UDP/443 is already end-to-end
encrypted, but the HTTP pairing channel carries share codes and bearer
tokens in plaintext.
