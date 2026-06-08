# fsend vs croc

Both tools do the same thing at the surface: share a short code, transfer
a file. They're CLI-first, no accounts, end-to-end encrypted, and run on
the major desktop OSes. If you've used one, the other will feel familiar.

The differences show up **when the two computers are on different
networks** — the common case, and where the two designs diverge.

## TL;DR

- **Across the internet, fsend goes directly between the two computers
  when it can; croc always goes through a relay.** This changes
  throughput, network reach, and what a third-party server can observe.
- **fsend uses QUIC over a single UDP port (UDP/443).** croc uses TCP
  over a five-port range (9009–9013). One UDP port is easier to get past
  a corporate firewall, and UDP/443 is allowed almost everywhere because
  it's the port HTTP/3 uses.
- **fsend has two independent crypto layers** (TLS 1.3 + SPAKE2) with
  **post-quantum** key exchange. croc has a single AEAD keyed from PAKE.

## Same network: no meaningful difference

On the same Wi-Fi, both tools discover each other locally over multicast
and transfer directly. Nothing crosses the internet.

```
   ┌────────┐                                                  ┌──────────┐
   │        │◄─────────── multicast discovery ────────────────►│          │
   │ Sender │                                                  │ Receiver │
   │        │◄═══════════ direct LAN transfer ════════════════►│          │
   └────────┘                                                  └──────────┘
```

fsend uses mDNS; croc uses its own IPv4/IPv6 multicast peer discovery.
The result is the same: bytes go straight over the LAN at link speed.
If your transfers never leave the local network, the two tools are
interchangeable on this dimension.

## Across the internet: the interesting case

Most real-world transfers cross the internet. Here the two tools take
fundamentally different paths.

### fsend: direct first, relay only as fallback

```
   ┌────────┐                ┌─────────────────┐                ┌──────────┐
   │ Sender │ ── intro ────► │ Pairing server  │ ◄─── intro ─── │ Receiver │
   └───┬────┘                │  (matchmaker)   │                └────┬─────┘
       │                     └─────────────────┘                     │
       │                                                             │
       └═══════════════════ direct P2P ══════════════════════════════┘
              (NAT hole-punched via ICE peer-to-peer
              connectivity checks; STUN-protocol messages
              between the two clients, not against a server)
```

The pairing server's only job is to introduce the two peers. They swap
NAT-traversal candidates through it, and as soon as one path works,
**bytes flow straight from sender to receiver** at whatever the two
internet links support. The server never sees a file byte.

When NAT topology makes hole-punching impossible (hard symmetric NATs,
some locked-down corporate networks), the server falls back to
forwarding encrypted UDP between the peers:

```
   ┌────────┐                 ┌─────────────────┐                 ┌──────────┐
   │ Sender │ ═══════════════►│ Pairing server  │ ═══════════════►│ Receiver │
   └────────┘   opaque UDP    │   (relay mode)  │   opaque UDP    └──────────┘
                              └─────────────────┘
                  (TLS terminates at the peers — server can't decrypt)
```

Even on this fallback path, the TLS session terminates at the peers, so
the server is moving bytes it can't decrypt.

### croc: always through a relay

```
   ┌────────┐                  ┌──────────────┐                  ┌──────────┐
   │ Sender │ ════════════════►│ Public relay │ ════════════════►│ Receiver │
   └────────┘   every byte     │  TCP, 9009+  │   every byte     └──────────┘
                               └──────────────┘
                          (default: croc.schollz.com)
```

croc has no NAT-traversal layer. Every cross-network byte makes **two
trips across the internet** — sender → relay → receiver — over multiple
TCP connections opened in parallel. Three things follow:

| Consequence       | Why it matters                                                          |
|-------------------|-------------------------------------------------------------------------|
| **Throughput cap** | Bounded by the relay's bandwidth, not your own links.                  |
| **Latency**       | Round-trip is sender → relay → receiver, even when peers are nearby.    |
| **Data path**     | Every cross-network byte (encrypted) flows through a third-party server. |

The relay sees only ciphertext once the PAKE handshake completes — but
it does see it, every time. fsend's relay sees the same opaque
ciphertext, but only on the fallback path; in the common case it sees
nothing at all.

## Transport and ports

|                       | fsend                                  | croc                                       |
|-----------------------|----------------------------------------|--------------------------------------------|
| Transport             | QUIC over UDP                          | TCP                                        |
| Default port(s)       | UDP/443                                | TCP/9009–9013                              |
| Parallelism           | QUIC streams multiplex over one socket | One TCP connection per relay port          |
| Firewall footprint    | One UDP port                           | Five TCP ports                             |
| IPv6                  | ✓                                      | ✓ (IPv6-first with IPv4 fallback)          |

Both designs parallelize for throughput, just differently. fsend rides
on QUIC, which multiplexes parallel streams over one UDP socket. croc
opens one TCP connection per port in the relay's banner (typically
9009–9013) and transfers data across them in parallel.

The practical consequence is firewall reach. UDP/443 is the port HTTP/3
uses, so it tends to be open on networks that allow modern web traffic.
The 9009–9013 range is frequently blocked by corporate egress filters;
croc users behind such networks typically rely on its `--socks5`
proxying support.

## Cryptography

Both tools are end-to-end encrypted: an attacker watching the wire (or
operating the server) cannot read your file. The strength of that
guarantee differs in three ways.

|                              | fsend                                                                         | croc                                                          |
|------------------------------|-------------------------------------------------------------------------------|---------------------------------------------------------------|
| End-to-end encrypted         | ✓                                                                             | ✓                                                             |
| PAKE protocol                | SPAKE2 (RFC 9382, IETF-standardized)                                          | `schollz/pake` (Boneh-Shoup textbook construction)            |
| Defense-in-depth layers      | **Two** independent — TLS 1.3 (QUIC) **+** SPAKE2 channel-bound to the TLS handshake (RFC 5705) | **One** — AEAD keyed directly from PAKE        |
| AEAD                         | TLS 1.3 ciphers (AES-128-GCM, ChaCha20-Poly1305, AES-256-GCM)                 | AES-256-GCM or XChaCha20-Poly1305 (selectable)                |
| Key derivation               | TLS 1.3 KDF + SPAKE2 + RFC 5705 exporter binding                              | PBKDF2-SHA256 (100 iterations) or Argon2id (64 MiB)           |
| Post-quantum forward secrecy | ✓ X25519 + ML-KEM-768 hybrid (NIST FIPS 203)                                  | ✗ Classical elliptic-curve only                               |
| MITM defense                 | TLS catches a network MITM; SPAKE2 binding catches a TLS-handshake MITM       | Single layer — PAKE only                                      |
| Relay can decrypt            | ✗ (relay path only) — TLS terminates at the peers                             | ✗ — ciphertext after PAKE                                     |

**The two-layer point.** A network attacker has to defeat both TLS 1.3
*and* SPAKE2 to read an fsend transfer — and SPAKE2 is bound to the TLS
handshake, so swapping in a forged TLS certificate breaks the binding
and is detected. croc relies on a single PAKE-derived AEAD; defeating
that one layer compromises the transfer.

**The post-quantum point.** fsend's TLS 1.3 key exchange uses Go's
X25519 + ML-KEM-768 hybrid. Ciphertext recorded today is not
retroactively decryptable by a future large-scale quantum computer.
croc uses classical elliptic-curve crypto only, so any recorded
ciphertext is vulnerable once a sufficient quantum computer exists.

For the full threat-model discussion, see [Security](security.md).

## Share codes

|                  | fsend                                | croc                                        |
|------------------|--------------------------------------|---------------------------------------------|
| Format           | `abc-defg-jkm` (3-4-3 letters)       | `1234-curious-iron-yellow` (PIN + words)    |
| Alphabet         | 23 letters (excludes `i`, `l`, `o`)  | digits + mnemonic wordlist                  |
| Entropy          | ~45 bits (log₂(23¹⁰))                | ~45 bits (10⁴ PIN + 32-bit mnemonic)        |
| One-shot         | ✓ — same code can't be reused        | Not documented                              |
| Server-side TTL  | 1 h unclaimed / 10 min after pairing | None documented                             |
| Custom secret    | `--pass` for a persistent password   | `--code` for a custom phrase (min 6 chars)  |

The codes carry comparable entropy. The operational difference is
fsend's documented one-shot semantics and bounded server-side TTL: a
leaked code is useful for at most one receiver, lives only as long as
the sender keeps the process running, and is hard-capped at one hour by
the server. croc's README and source do not specify equivalent
server-side guarantees, and `--code` lets a sender reuse a chosen
phrase across many invocations.

## Feature parity

Day-to-day, the surface is very similar:

| Capability                        | fsend         | croc                  |
|-----------------------------------|---------------|-----------------------|
| Send files                        | ✓             | ✓                     |
| Send folders                      | ✓             | ✓ (`--zip` to compress)|
| Multiple files in one transfer    | ✓             | ✓                     |
| Send from stdin                   | ✓             | ✓                     |
| Send a literal string             | `--text`      | `--text`              |
| Receiver confirmation prompt      | ✓ (`--yes`)   | ✓ (`--yes`)           |
| Resume after interruption         | ✓             | ✓                     |
| Password-gate the transfer        | `--pass`      | (code phrase doubles as the secret) |
| Exclude paths when sending a dir  | `--exclude`   | `--exclude`           |
| Custom output dir                 | `--out`       | `--out`               |
| QR code for mobile receivers      | ✗             | ✓ (`--qrcode`)        |
| SOCKS5 proxy                      | ✗             | ✓ (`--socks5`)        |
| Custom elliptic curve             | n/a (TLS 1.3) | ✓ (`--curve`)         |
| Self-host the server / relay      | ✓             | ✓                     |

## When croc is the better fit

- You're already happy with croc and your workflow depends on
  `--qrcode`, `--socks5`, `--curve`, or `--zip`.
- You're running on a network that blocks UDP but allows TCP/9009.
- You're already self-hosting a croc relay and don't want to migrate.

## When fsend is the better fit

- Most of your transfers cross the internet and **throughput matters**
  — direct P2P uses both peers' full bandwidth, not a relay's.
- You're on a network that blocks the croc port range but allows
  UDP/443.
- You care about long-term confidentiality (post-quantum forward
  secrecy) or defense-in-depth (two independent crypto layers).
- You want stronger code-handling guarantees: one-shot by design and a
  bounded server-side TTL (1 hour max unclaimed, 10 min after pairing).

## Summary

|                                      | fsend                                              | croc                                              |
|--------------------------------------|----------------------------------------------------|---------------------------------------------------|
| **Cross-network data path**          | Direct when hole-punching succeeds; relay fallback | Always relayed                                    |
| **Cross-network speed cap**          | Your two internet links (when direct)              | The relay's bandwidth (always)                    |
| **Server sees ciphertext**           | Only on relay fallback                             | Every cross-network transfer                      |
| **Same-LAN transfer**                | Direct via mDNS                                    | Direct via multicast peer discovery               |
| **Transport**                        | QUIC over UDP                                      | TCP                                               |
| **Firewall footprint**               | One UDP port (UDP/443)                             | Five TCP ports (9009–9013)                        |
| **PAKE**                             | SPAKE2 (RFC 9382)                                  | `schollz/pake`                                    |
| **Defense-in-depth layers**          | Two (TLS 1.3 + SPAKE2, channel-bound)              | One (AEAD)                                        |
| **Post-quantum forward secrecy**     | ✓ X25519 + ML-KEM-768                              | ✗                                                 |
| **Code format**                      | `abc-defg-jkm`                                     | `1234-word-word-word`                             |
| **Code is one-shot**                 | ✓ (server-enforced)                                | Not specified                                     |
| **Code expiry**                      | 1 h unclaimed / 10 min after pairing               | Not specified                                     |
| **Resume after interruption**        | ✓                                                  | ✓                                                 |
| **Self-hostable**                    | ✓ (single binary, no database)                     | ✓                                                 |

For the architectural deep dive, see [How it works](architecture.md).
For the cryptographic deep dive, see [Security](security.md).
