# fsend

Truly peer-to-peer file transfer — direct, encrypted, relay only as a fallback. 🚀🔒

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
  machines and the pairing server only helped you find each other.
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
know the code can't decrypt captured traffic from either, and **neither
relay can read your filenames, file sizes, hashes, or content**. The
differences are in the cryptographic substrate underneath.

|                                  | fsend                                              | croc                                              |
|----------------------------------|----------------------------------------------------|---------------------------------------------------|
| End-to-end encrypted             | ✓                                                  | ✓                                                 |
| PAKE protocol                    | SPAKE2 (RFC 9382, IETF-standardized)               | `schollz/pake` (non-standardized, Boneh-Shoup textbook construction) |
| Crypto layers over the wire      | TLS 1.3 over QUIC **plus** SPAKE2, channel-bound via the RFC 5705 exporter | Single AEAD (AES-GCM or XChaCha20-Poly1305) keyed directly from PAKE |
| Post-quantum forward secrecy     | ✓ (X25519 + ML-KEM-768 hybrid, Go 1.24+ default)   | ✗ (classical ECC only)                            |
| MITM defense                     | Two layers — TLS catches a network MITM, SPAKE2 binding catches a TLS-handshake MITM | Single layer — PAKE alone                         |
| Relay sees the file              | ✗ — relay only forwards opaque UDP datagrams; QUIC/TLS terminate at the peers | ✗ — relay only forwards ciphertext after PAKE     |
| How often the relay is in path   | Fallback only (used when ICE hole-punching fails)  | Every cross-network transfer                      |

The two-layer story is the load-bearing one: in fsend you'd have to
break **both** TLS 1.3 and SPAKE2 to compromise a transfer; in croc you
only have to break the one PAKE-derived AEAD. And because fsend's
TLS 1.3 layer uses a post-quantum hybrid KEX by default, ciphertext
captured today is not retroactively decryptable by a future large-scale
quantum computer — recorded croc traffic is.

## How it works

When you run `fsend file.pdf`, the sender opens **two paths at the same
time** — a LAN listener (mDNS-announced) and a session on the pairing
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

Pairs in well under a second. Bytes never touch the pairing server or
cross NAT. Works even if the pairing server is offline.

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
symmetric NAT, some locked-down corporate networks), the pairing
server forwards encrypted UDP datagrams between the peers — it never
sees plaintext, since TLS terminates at the peers, not at the server.

### Why this design

The two paths run concurrently rather than sequentially because waiting
is always wrong for one of the two cases: same-network users would
needlessly wait before the cross-network path even starts, and
cross-network users would be blocked behind a same-network attempt that
will never succeed. Running both at once gives you the fastest answer
either way.

### When the pairing server is unreachable

Same-LAN transfers continue working — the LAN path doesn't depend on the
server. The sender surfaces a one-line warning so you know cross-network
receivers can't connect right now:

```
⚠ Server unreachable — only same-LAN receivers can connect.
```

You can keep transferring on the local network or use `fsend --connect
<other-host>` to point at a different server.

## Is the pairing server something to worry about?

No. It's a matchmaker, not a middleman — and even when it has to step in
as a fallback, it can't read your files. Here's the full picture.

### Why does the server need to exist at all?

Two machines on different networks usually can't see each other. Both
sit behind home or office routers (NAT) that hide them from the public
internet, so neither side knows where to send the first packet. They
need a meeting point to swap addresses before they can talk directly.

```
   Sender                   Pairing server                  Receiver
      │                  ┌───────────────────┐                │
      │  "I'm here,      │   matchmaker:     │  "I have code  │
      │   code abc-..."  │   pair two peers  │   abc-..."     │
      │ ────────────────►│   who share the   │◄────────────── │
      │                  │   same code       │                │
      │                  └─────────┬─────────┘                │
      │                            │                          │
      │   "here are each other's public addresses — go talk"  │
      │                            │                          │
      └────── direct peer-to-peer (server steps aside) ───────┘
```

That's the whole job: introduce the two peers, then get out of the way.
In the typical cross-network case the file flows peer-to-peer and the
server never sees a single byte of it.

When the two networks make a direct connection impossible (hard NAT,
locked-down corporate firewalls), the server falls back to forwarding
**already-encrypted** UDP datagrams between the peers — think of a mail
carrier moving sealed envelopes. It moves the parcel; it can't open it.

### What the server can and cannot see

|                                                   | Server sees     |
|---------------------------------------------------|-----------------|
| File contents                                     | ✗ never         |
| File names, sizes, hashes                         | ✗ never         |
| Encrypted ciphertext (on the relay-fallback path) | ✓ as opaque bytes — not decryptable, not even by the server's operator |
| The 7-character pairing code & your IP            | ✓ briefly, in memory only, for pairing — never written to disk |

End-to-end encryption (TLS 1.3 + SPAKE2, see the [Security](#security)
section) means even the operator of the server can't decrypt traffic
that goes through it. The encryption keys never leave the two peers.

### What it writes to disk

Effectively nothing:

- **No access log. No per-transfer log line.** The default log level
  only emits lifecycle events — startup, shutdown, and errors.
- **No IP addresses or pairing codes in logs**, at any level.
- **No database, no persistence layer.** Pairing state lives in RAM,
  evicts within an hour at most (ten minutes once a transfer has
  paired), and is gone forever on restart.

If you'd still rather not trust our public server,
[self-host one](#self-hosting) — it's a single binary with nothing to
back up.

## Self-hosting

The default pairing server at `fs.alzina.dev` is best-effort. To run
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
