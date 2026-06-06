# fsend

Truly peer-to-peer file transfer over QUIC, with NAT hole-punching and
end-to-end encryption authenticated from a short shared code.

```bash
# Send
fsend report.pdf

# Receive (on the other machine)
fsend abc-defg-jkm
```

See **[USAGE.md](USAGE.md)** for every flag, environment variable, and exit
code — including folders, stdin, password-gated transfers, and self-hosted
servers.

## Install

```bash
curl -fsSL https://fs.alzina.dev/install.sh | sh
```

Or download a release binary from [the releases page](https://github.com/polius/fsend/releases).

## How it differs from croc

Both tools work the same way at the surface: share a short code, transfer
a file. The big difference shows up **when the two machines are on
different networks** — the hard case, and the one most people care about.

**Same LAN**, both tools behave the same: peers find each other via local
UDP multicast and connect directly. No meaningful difference.

**Across the internet**, the two tools diverge:

- **croc** has no NAT traversal. Cross-network transfers always go through
  croc's public relay — every byte makes two trips across the internet
  (you → relay → them), so the speed is capped by the relay's bandwidth
  no matter how fast your own connections are.
- **fsend** hole-punches through both NATs first using ICE + STUN. When
  hole-punching succeeds, bytes flow **directly** between the two
  machines and the rendezvous server only helped you find each other.
  fsend only falls back to a relay when NAT topology makes hole-punching
  impossible (typically symmetric NATs and some locked-down corporate
  networks).

### fsend across the internet

```
   ┌─ NAT hole-punching works  (the common case)
   │     └─► direct peer-to-peer between the two machines
   │         (server matched you up, never touches the bytes)
   │
   └─ Hole-punching blocked  (hard NAT, locked-down networks)
         └─► encrypted UDP relay through the server
             (server forwards ciphertext only — never sees the file)
```

### croc across the internet

```
   You ───►  public relay  ───►  Them      (every cross-network byte)
```

### Side by side

|                                  | fsend                                              | croc                                              |
|----------------------------------|----------------------------------------------------|---------------------------------------------------|
| Cross-network path               | direct when hole-punching succeeds                 | always relayed (no NAT traversal)                 |
| Cross-network speed cap          | your two internet links (when direct)              | the relay's bandwidth (always)                    |
| Same-LAN transfers               | direct, via mDNS multicast                         | direct, via UDP multicast                         |
| Server touches your bytes        | only when hole-punching is blocked                 | every cross-network transfer                      |
| Transport                        | QUIC over UDP                                      | TCP                                               |
| Firewall footprint               | **one UDP port** (UDP/443 by default)              | five TCP ports (9009–9013 by default)             |
| End-to-end encryption            | TLS 1.3 + SPAKE2 (RFC 9382) bound to the code      | PAKE + AES-GCM or XChaCha20-Poly1305              |
| Resume after interruption        | ✓                                                  | ✓                                                 |

The **single UDP port** is a real-world advantage: one port is much
easier to get past a corporate firewall than a TCP range, and UDP/443 is
already allowed almost everywhere because it's the port HTTP/3 uses.
QUIC makes this possible by multiplexing all the transfer's streams over
one UDP socket, where TCP would need a separate connection per stream.

### What this means in practice

- **Throughput.** A relayed transfer is bounded by the relay's available
  bandwidth. A direct transfer is bounded by the two peers' own links.
- **Network reach.** The TCP port range croc uses by default
  (9009–9013) is often blocked by corporate egress filters. UDP/443 is
  the port HTTP/3 uses and is almost universally allowed.
- **Data path.** On the relay path, ciphertext for every cross-network
  transfer transits a third-party server. On the direct path, it does
  not. Both paths are end-to-end encrypted.

On the same LAN the two tools take the same direct path; the
differences above only apply to cross-network transfers.

### Security

Both tools end-to-end encrypt the file: a network observer who doesn't
know the code can't decrypt captured traffic from either. Two
implementation differences are worth flagging:

- **PAKE protocol.** fsend uses symmetric SPAKE2 (RFC 9382), an
  IETF-standardized password-authenticated key exchange. croc's PAKE
  library (`schollz/pake`) implements a non-standardized construction
  derived from Boneh & Shoup's cryptography textbook (pg. 789).
  No public attack is known against either; the difference is the
  protocol's standardization and peer-review surface.
- **Data-channel encryption.** fsend transports file bytes inside a
  TLS 1.3 session over QUIC, with the SPAKE2-derived secret bound to
  that session via the RFC 5705 exporter — this defeats an active MITM
  on the self-signed TLS handshake. The Go toolchain fsend builds with
  (≥ 1.24) defaults the TLS 1.3 key exchange to a post-quantum hybrid
  (X25519 + ML-KEM-768), so ciphertext recorded today is not
  retroactively decryptable by a future large-scale quantum computer.
  croc encrypts file bytes directly with a PAKE-derived AEAD (AES-GCM
  or XChaCha20-Poly1305) using classical elliptic-curve cryptography
  only.

## How it works

When you run `fsend file.pdf`, the sender opens **two paths at the same
time** — a LAN listener (mDNS-announced) and a session on the rendezvous
server. Whichever path the receiver reaches first wins; the other is
cancelled. There is **no timeout** between the two — neither side ever
waits on a budget.

### Same network (sender and receiver on the same Wi-Fi)

```
   Sender                                  Receiver
   fsend report.pdf                        fsend abc-defg-jkm
     │                                       │
     ├─ LAN listener  ──────────────►  ◄──── mDNS query (300 ms)
     │                                       │
     └─ Server register (standby)            └─ HIT → dial LAN port
                                                       │
                                                       ▼
                                          direct P2P over LAN
                                          (server path cancelled)
```

Pairs in well under a second. Bytes never touch the rendezvous server or
cross NAT. Works even if the rendezvous server is offline.

### Different networks (sender at home, receiver at a café)

```
   Sender                                  Receiver
   fsend report.pdf                        fsend abc-defg-jkm
     │                                       │
     ├─ LAN listener (no one comes)          ├─ mDNS query (300 ms, miss)
     │                                       │
     └─ Server register  ──────────► ◄────── └─ Join server
                                                       │
                                                       ▼
                                            ICE hole-punch (common case)
                                            ─── or, on hard NAT ───
                                            UDP relay (encrypted, opaque)
                                            (LAN path cancelled)
```

When the two NATs can be punched through, bytes flow directly between
the two peers. When NAT topology makes hole-punching impossible (hard
symmetric NAT, some locked-down corporate networks), the rendezvous
server forwards encrypted UDP datagrams between the peers — it never
sees plaintext, since TLS terminates at the peers, not at the server.

### Why this design

The two paths run concurrently rather than sequentially because waiting
is always wrong for one of the two cases: same-network users would
needlessly wait before the cross-network path even starts, and
cross-network users would be blocked behind a same-network attempt that
will never succeed. Running both at once gives you the fastest answer
either way.

### When the rendezvous server is unreachable

Same-LAN transfers continue working — the LAN path doesn't depend on the
server. The sender surfaces a one-line warning so you know cross-network
receivers can't connect right now:

```
⚠ Rendezvous server unreachable — only same-LAN receivers can connect.
```

You can keep transferring on the local network or use `fsend --connect
<other-host>` to point at a different server.

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
**not** recommended on the public internet):

| Port             | Direction | Purpose                                         |
|------------------|-----------|-------------------------------------------------|
| `8080/tcp`       | inbound   | Signaling HTTP API (clients POST session/join)  |
| `443/udp`        | inbound   | ICE STUN-style reflection + relay fallback (opaque QUIC datagrams) |

Clients then connect with `fsend --connect http://host:8080`. If you change
`FSEND_UDP_ADDR`, also set `FSEND_PUBLIC_ADDR=host:port` to the address clients
should dial for relay.

**HTTPS mode** (reverse proxy with your own domain — recommended for any
public-internet deployment; matches the `deploy/compose/` stack):

| Port             | Direction | Purpose                                              |
|------------------|-----------|------------------------------------------------------|
| `443/tcp`        | inbound   | HTTPS signaling — terminated by Caddy/nginx/Traefik  |
| `443/udp`        | inbound   | ICE STUN-style reflection + relay fallback — goes **directly** to fsend-server |
| `80/tcp`         | inbound   | Let's Encrypt ACME HTTP-01 challenge (cert issue/renew) |
| `8080/tcp`       | internal  | fsend-server signaling — only reachable by the proxy |

Clients then connect with `fsend --connect fs.example.com:443` (HTTPS is the
default scheme for non-local hosts). TCP/443 and UDP/443 share the same port
number but are different protocols, so both can bind simultaneously.

No outbound ports beyond what your OS/Docker needs. fsend-server makes no
outbound connections to clients.

## Documentation

- [Usage reference](USAGE.md) — every flag, env var, and exit code
- [Development guide](DEVELOPMENT.md) — building and testing from source

## License

MIT
