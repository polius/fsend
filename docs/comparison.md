# fsend vs croc

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

## fsend across the internet

```
   ┌─ NAT hole-punching works  (the common case)
   │     └─► direct peer-to-peer between the two machines
   │         (server matched you up, never touches the bytes)
   │
   └─ Hole-punching blocked  (hard NAT, locked-down networks)
         └─► encrypted UDP relay through the server
             (server forwards ciphertext only — never sees the file)
```

## croc across the internet

```
   You ───►  public relay  ───►  Them      (every cross-network byte)
```

## Side by side

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

## What this means in practice

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

For the cryptographic comparison, see [Security](security.md).
