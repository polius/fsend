# fsend

Truly peer-to-peer file transfer over QUIC, with NAT hole-punching and
end-to-end encryption authenticated from a short shared code.

```bash
# Send
fsend report.pdf

# Receive (on the other machine)
fsend abc-defgh-jkm
```

## Install

```bash
curl -fsSL https://fs.alzina.dev/install.sh | sh
```

Or download a release binary from [the releases page](https://github.com/polius/fsend/releases).

## How it differs from croc

fsend goes **direct peer-to-peer** for the dominant cross-NAT case, where
croc always relays. See [PROJECT_SPEC.md](PROJECT_SPEC.md) for the full
design rationale.

- QUIC over a single UDP port (vs croc's TCP port range)
- ICE hole-punching with `pion/ice` (vs croc's no NAT traversal)
- SPAKE2 (RFC 9382) channel-bound to TLS 1.3 (X25519 + ML-KEM-768 hybrid)
- BLAKE3 chunk-verified streaming with resume
- Auto-detect compression (zstd when it helps, raw when it doesn't)

## Self-hosting

The default rendezvous server at `fs.alzina.dev` is best-effort. To run
your own, see [`deploy/compose/`](deploy/compose/).

```bash
export FSEND_DOMAIN=fs.example.com
docker compose -f deploy/compose/docker-compose.yml up -d
fsend --connect fs.example.com:443    # on each client
```

### Ports

fsend-server has two listeners: a **TCP HTTP signaling API** (`FSEND_HTTP_ADDR`,
default `:8080`) and a **UDP relay** (`FSEND_UDP_ADDR`, default `:443`). The
UDP relay only carries opaque QUIC ciphertext between peers — TLS terminates at
the peers, not at the server — so it is the same listener whether or not you put
HTTPS in front of signaling.

**HTTP-only mode** (no reverse proxy — fine for LAN, dev, or trusted networks;
**not** recommended on the public internet, see [threat model](docs/security/threat-model.md)):

| Port             | Direction | Purpose                                         |
|------------------|-----------|-------------------------------------------------|
| `8080/tcp`       | inbound   | Signaling HTTP API (clients POST session/join)  |
| `443/udp`        | inbound   | Relay fallback (opaque QUIC datagrams)          |

Clients then connect with `fsend --connect http://host:8080`. If you change
`FSEND_UDP_ADDR`, also set `FSEND_PUBLIC_ADDR=host:port` to the address clients
should dial for relay.

**HTTPS mode** (reverse proxy with your own domain — recommended for any
public-internet deployment; matches the `deploy/compose/` stack):

| Port             | Direction | Purpose                                              |
|------------------|-----------|------------------------------------------------------|
| `443/tcp`        | inbound   | HTTPS signaling — terminated by Caddy/nginx/Traefik  |
| `443/udp`        | inbound   | Relay fallback — goes **directly** to fsend-server   |
| `80/tcp`         | inbound   | Let's Encrypt ACME HTTP-01 challenge (cert issue/renew) |
| `8080/tcp`       | internal  | fsend-server signaling — only reachable by the proxy |

Clients then connect with `fsend --connect fs.example.com:443` (HTTPS is the
default scheme for non-local hosts). TCP/443 and UDP/443 share the same port
number but are different protocols, so both can bind simultaneously.

No outbound ports beyond what your OS/Docker needs. fsend-server makes no
outbound connections to clients.

## Documentation

- [Project spec](PROJECT_SPEC.md) — architecture, decisions, UX
- [Threat model](docs/security/threat-model.md) — what we defend against
- [Privacy](docs/security/privacy.md) — no telemetry, no logs

## License

MIT
