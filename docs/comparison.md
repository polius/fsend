# fsend vs croc vs magic-wormhole

A complete, side-by-side comparison of the three leading code-based,
end-to-end-encrypted CLI file-transfer tools.

Every row below was verified against the **actual source code** of each
project, not its marketing — fsend at this repo, croc at
[`schollz/croc`](https://github.com/schollz/croc) v10.4.4, and
magic-wormhole at
[`magic-wormhole/magic-wormhole`](https://github.com/magic-wormhole/magic-wormhole)
v0.24.0. Where a tool's README and its code disagree, the code wins and
it's flagged.

All three share the same surface: share a short code, transfer a file,
no accounts, end-to-end encrypted, self-hostable. The differences are in
**how the bytes actually move**, **how strong the encryption is**, and
**what the day-to-day feature set covers**.

> **Last verified June 2026** against the source revisions listed below.
> If any row is out of date, please
> [open an issue](https://github.com/polius/fsend/issues) and it'll be
> corrected.

## TL;DR

- **Direct peer-to-peer across the internet.** fsend hole-punches
  through NATs (ICE) and sends bytes straight between the two machines at
  their own bandwidth. **croc always relays across the internet.**
  **magic-wormhole relays whenever both peers are behind ordinary NATs**
  (no hole-punching). On the common home-to-café transfer, fsend is the
  only one that *can* keep a third-party server out of the data path —
  when hole-punching succeeds; it falls back to a relay when NAT topology
  defeats it.
- **Strongest cryptography.** fsend is the only one of the three with
  **post-quantum** key exchange (X25519 + ML-KEM-768) and **two
  independent** security layers (TLS 1.3 *and* SPAKE2 bound to the TLS
  handshake). croc and magic-wormhole each have a single classical layer.
- **Hardest codes to brute-force by default.** fsend rate-limits guesses
  server-side and the code never reaches the server; its ~45 bits of
  entropy match croc's. magic-wormhole's default is **16 bits** with no
  rate limiting (its own threat model documents a 1-in-65536 guess
  chance).
- **Works on a LAN with no internet.** fsend (mDNS) and croc (multicast)
  both discover peers locally and transfer with no internet at all;
  **magic-wormhole can't** — it must reach its central mailbox server to
  pair, even on the same Wi-Fi.
- **One self-contained static binary.** fsend and croc are single Go
  binaries; magic-wormhole is a Python program needing Python ≥ 3.10 and
  a dependency stack.

## At a glance

|                                   | **fsend**                                  | **croc**                              | **magic-wormhole**                          |
|-----------------------------------|--------------------------------------------|---------------------------------------|---------------------------------------------|
| End-to-end encrypted              | ✓                                          | ✓                                     | ✓                                           |
| Cross-network data path           | **Direct P2P** (relay fallback)            | Always relayed                        | Direct only if a peer is reachable; else relayed |
| NAT hole-punching                 | **✓ ICE (STUN)**                           | ✗                                     | ✗                                           |
| Cross-network speed cap           | **Your two internet links** (when direct)  | The relay's bandwidth (always)        | The relay's bandwidth (NAT-to-NAT)          |
| LAN transfer without internet     | **✓ mDNS, server offline**                 | **✓ multicast (relay skipped on LAN)**| ✗ (mailbox always required to pair)         |
| Transport (bulk)                  | **QUIC + TLS 1.3** over UDP                 | Raw TCP + AES-256-GCM                  | Raw TCP + NaCl secretbox                    |
| Servers involved                  | **One** (pairing; relay only on fallback)  | One (relay; always in path)           | Two (mailbox + transit relay)               |
| Firewall footprint                | **One UDP port (UDP/443)**                  | Five TCP ports (9009–9013)            | wss/443 + TCP/4001                          |
| Defense-in-depth layers           | **Two** (TLS 1.3 + SPAKE2, channel-bound)  | One (AEAD from PAKE)                   | One (secretbox from SPAKE2)                  |
| MITM protection                   | **Automatic** (channel-bound)              | PAKE only                             | Manual `--verify`                           |
| Post-quantum forward secrecy      | **✓ X25519 + ML-KEM-768**                  | ✗                                     | ✗                                           |
| Default code entropy              | **~45 bits**                               | **~45 bits**                          | 16 bits (adjustable)                        |
| Online-guess protection           | **Rate-limited 30/min + bounded TTL**      | None server-side                      | None (acknowledged in docs)                 |
| Password on top of the code       | **✓ `--pass`**                             | ✗ (code is the secret)                | ✗ (code is the secret)                      |
| Choose your own code phrase       | ✗                                          | **✓ `--code`**                        | **✓ `--code`**                              |
| Resume after interruption         | **✓ (BLAKE3 chunk-verified)**              | **✓ (chunk-based)**                   | ✗ (classic transit restarts)               |
| Skip unchanged files on re-send   | **✓ (size+mtime; `--checksum` to hash)**   | **✓ (always hashes)**                 | ✗                                           |
| Multiple files per transfer       | **✓**                                       | **✓**                                 | ✗ (one file/dir per code)                   |
| Distribution                      | **Single static binary**                    | **Single static binary**              | Python package (pip / OS repos)             |
| License                           | MIT                                        | MIT                                   | MIT                                         |

## How the bytes move (the decisive difference)

Most real transfers cross the internet, between two machines that are
each behind a home or office router (NAT). Neither can see the other
directly, so something has to introduce them. What happens *after* the
introduction is where the three tools fundamentally diverge.

### fsend — direct first, relay only as a true fallback

The pairing server is a matchmaker: both peers register, swap NAT-
traversal candidates through it (ICE), and as soon as one hole-punched
path works, **bytes flow straight from sender to receiver** at whatever
the two links support. The server never sees a file byte. Only when NAT
topology defeats hole-punching (hard symmetric NAT, locked-down corporate
networks) does the server fall back to forwarding encrypted UDP it can't
decrypt.

```
   Sender ── intro ──►  Pairing server  ◄── intro ── Receiver
      │                 (matchmaker)                      │
      └════════════════ direct P2P ════════════════════════┘
              (NAT hole-punched via ICE; server steps aside)
```

### croc — always through the relay

croc has no NAT-traversal layer at all (no STUN, no ICE — confirmed
absent from its source). Every cross-network byte makes two trips across
the internet: sender → relay → receiver, over four parallel TCP
connections by default. The public relay is `croc.schollz.com`.

```
   Sender ════════►  Public relay (TCP 9009)  ════════►  Receiver
              every byte, every transfer
```

### magic-wormhole — direct only if a peer is already reachable

magic-wormhole's transit layer tries a direct TCP connection using
"hints" (each side's local IPs + a random listening port). This works
when a peer is *already reachable* — same LAN, a public IP, or a manually
forwarded port — but there is **no hole-punching** (STUN-assisted
traversal is listed as a future idea, not shipped). So two peers behind
ordinary NATs fall back, after a few seconds, to the transit relay
`transit.magic-wormhole.io:4001`.

```
   Sender ════════►  Transit relay (TCP 4001)  ════════►  Receiver
              when both peers are behind NAT
```

**Consequences of relaying** (apply to croc always, and to
magic-wormhole on every NAT-to-NAT transfer):

| Consequence       | Why it matters                                                        |
|-------------------|-----------------------------------------------------------------------|
| Throughput cap    | Bounded by the relay's bandwidth, not your own links.                 |
| Latency           | Round-trip is sender → relay → receiver, even when peers are nearby.  |
| Data path         | Every cross-network byte (encrypted) flows through a third party.     |

In all three the relay sees only ciphertext. The difference is *how
often* a third-party server is in the data path: **fsend, only on the
rare hole-punch-failure fallback; croc, always; magic-wormhole, on every
NAT-to-NAT transfer.**

## Same network (both on the same Wi-Fi)

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Local discovery | **mDNS (Pion)** | **IPv4/IPv6 multicast (`239.255.255.250`)** | None |
| Bytes go directly over the LAN | ✓ | ✓ | ✓ (transit hints) |
| Pairing works with the server/internet **offline** | **✓** | **✓ (multicast)** | ✗ — mailbox still required |

fsend and croc both discover the peer locally and transfer at link speed
with no internet. **magic-wormhole has no local discovery**: even on the
same Wi-Fi, the PAKE handshake still goes through the central mailbox
server over the internet — only the bulk transit connection is then made
directly. If your two machines share a LAN but have no internet (an
air-gapped office, a field deployment, a plane), fsend and croc work and
magic-wormhole does not.

## Transport and ports

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Bulk transport | QUIC over UDP, TLS 1.3 | Raw TCP | Raw TCP |
| Stream encryption | TLS 1.3 (AEAD) | AES-256-GCM | NaCl secretbox (XSalsa20-Poly1305) |
| Rendezvous channel | HTTP/3 over UDP/443 | TCP control connection on relay | WebSocket over TLS (wss/443) |
| Default port(s) | **UDP/443** (server); ephemeral for direct | TCP/9009–9013 (5 ports) | wss/443 + TCP/4001 |
| Parallelism | QUIC streams over one socket | One TCP conn per port (default 4 + 1 control) | Single transit connection |
| Firewall footprint | **One server-side UDP port** | Five TCP ports | Two ports (443 + 4001) |
| IPv6 | **✓** | **✓ (IPv6-first, IPv4 fallback)** | Best-effort (not first-class) |

The practical consequence is firewall reach. **UDP/443 is the port HTTP/3
uses**, so it tends to be open wherever modern web traffic is allowed.
croc's 9009–9013 range and magic-wormhole's TCP/4001 are non-standard
ports that corporate egress filters frequently block — and on
magic-wormhole, the wss/443 handshake can succeed while the relayed
transfer on 4001 fails to connect. (croc and magic-wormhole both offer
SOCKS5/Tor proxying as a workaround; fsend leans on UDP/443 being
near-universally reachable.)

## Cryptography

All three are end-to-end encrypted: an attacker on the wire — or
operating the server — cannot read your file. The strength of that
guarantee is where they separate.

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| End-to-end encrypted | ✓ | ✓ | ✓ |
| PAKE protocol | SPAKE2 (`gospake2`, symmetric / python-spake2 construction) | `schollz/pake` v3 (Boneh–Shoup textbook PAKE2) | SPAKE2 (`python-spake2`, symmetric mode) |
| Defense-in-depth layers | **Two independent** — TLS 1.3 (QUIC) **+** SPAKE2 channel-bound to the TLS handshake (RFC 5705) | One — AEAD keyed from PAKE | One — secretbox keyed from SPAKE2 |
| AEAD cipher | TLS 1.3 ciphers (AES-GCM / ChaCha20-Poly1305) | AES-256-GCM | NaCl secretbox (XSalsa20-Poly1305) |
| Key derivation | TLS 1.3 KDF + SPAKE2 + RFC 5705 exporter binding | PBKDF2-SHA256, 100 iterations (over the PAKE key) | HKDF-SHA256 from the SPAKE2 key |
| Post-quantum forward secrecy | **✓ X25519 + ML-KEM-768 (NIST FIPS 203)** | ✗ classical ECC only | ✗ classical only |
| MITM defense | TLS catches a network MITM; SPAKE2 binding catches a TLS-handshake MITM — **automatic** | Single PAKE layer | SPAKE2 + optional manual `--verify` string |
| Fresh keys per transfer | ✓ ephemeral Ed25519 cert (1 h) | PAKE session key per transfer | SPAKE2 session key per transfer |
| Integrity check | **BLAKE3 per-chunk + BLAKE3 root over the whole file** | per-message GCM tag; xxhash for resume/skip | per-record Poly1305 MAC (no whole-file hash) |
| Relay can decrypt | ✗ (relay path only) | ✗ | ✗ |

**Two layers vs one.** A network attacker against fsend must defeat
**both** TLS 1.3 *and* SPAKE2 — and because SPAKE2 is bound to the TLS
handshake via the RFC 5705 exporter, swapping in a forged certificate
breaks the binding and is caught automatically before any file data
flows. croc and magic-wormhole each rest on a single PAKE-derived key;
defeating that one layer compromises the transfer.

**Post-quantum.** Only fsend's key exchange is quantum-resistant.
Ciphertext recorded today off a croc or magic-wormhole transfer becomes
decryptable once a sufficiently large quantum computer exists; fsend's
X25519 + ML-KEM-768 hybrid is not.

## Share codes

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Format | `abc-defg-jkm` (3-4-3 letters) | `1234-word-word-word-word` (PIN + 4 words) | `7-crossover-clockwork` (nameplate + words) |
| Alphabet / wordlist | 23 letters (excludes `i`,`l`,`o`) | 4-digit PIN + 256-word mnemonic list | 256-word PGP word list |
| Default secret entropy | **~45 bits** | **~45 bits** (~13-bit PIN + ~32-bit words) | 16 bits (two words) |
| Adjustable length | Fixed | Fixed (custom phrase via `--code`) | **`--code-length N`** (~8 bits/word) |
| Custom code phrase | ✗ (system-generated) | **✓ `--code` (min 6 chars)** | **✓ `--code`** |
| Receiver allocates the code | ✗ | ✗ | **✓ `--allocate`** |
| One-shot | ✓ (can't be reused) | ✓ per session (reusable via `--code`) | ✓ nameplate single-use |
| Server-side TTL | **1 h unclaimed / 10 min after pairing** | 3 h room TTL | Not specified in client docs |
| Online-guess rate limiting | **30/min per source IP** | None server-side | None (acknowledged in docs) |
| Code reaches a server | **Never** — only an argon2id-stretched slot | The relay sees the room (SHA-256 of code prefix) | Words never; the (non-secret) nameplate does |

fsend takes the strong-by-default stance: the code never reaches the
server (both peers register under a 64-MiB argon2id-stretched *slot*
derived from it), it carries ~45 bits, and the public server rate-limits
new sessions — so online brute force is infeasible without the operator
having to configure anything. magic-wormhole's own
[threat model](https://magic-wormhole.readthedocs.io/en/latest/attacks.html)
spells out the cost of its 16-bit default: an attacker controlling the
network or mailbox has a **1-in-65536** chance per attempt, with no rate
limiting. The trade-off is flexibility — magic-wormhole (and croc) let
you choose your own code phrase; fsend always picks it for you.

## Features

| Capability | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Send files | ✓ | ✓ | ✓ |
| Send folders | ✓ | ✓ (`--zip` to compress) | ✓ (zipped, deflated) |
| Multiple files in one transfer | **✓** | **✓** | ✗ (one file/dir per code) |
| Send from stdin | **✓ (binary)** | **✓** | ✗ (`--text -` only) |
| Stream payload to stdout | **✓ `--out -`** | **✓ `--stdout`** | ✗ (text prints; files saved) |
| Send a literal string | `--text` | `--text` | `--text` |
| Receiver confirmation prompt | ✓ (`--yes` to skip) | ✓ (`--yes`) | ✓ (`--accept-file`) |
| Resume after interruption | **✓ (BLAKE3-verified)** | **✓ (chunk-based)** | ✗ (classic transit restarts) |
| Skip unchanged files on re-send | **✓ (size+mtime; `--checksum`)** | **✓ (always hashes)** | ✗ |
| Dry-run preview | **✓ `--dry-run`** | ✗ | ✗ |
| Password on top of the code | **✓ `--pass`** | code phrase doubles as secret | ✗ (the code is the secret) |
| Exclude paths in a directory | **✓ `--exclude`** | **✓ `--exclude` (+ `--git`)** | ✗ |
| Custom output dir / name | `--out` | `--out` | `--output-file` |
| Compression | zstd (always) | DEFLATE (`--no-compress` to disable) | DEFLATE (folders only) |
| Bandwidth throttle | ✗ | **✓ `--throttleUpload`** | ✗ |
| QR code for the code | ✗ | **✓ `--qrcode`** | **✓ (on by default)** |
| SOCKS5 proxy | ✗ | **✓ `--socks5`** | — |
| Tor support | ✗ | via proxy | **✓ `--tor` / `--launch-tor`** |
| Custom elliptic curve | n/a (TLS 1.3) | **✓ `--curve`** | ✗ |
| SSH-key transfer helper | ✗ | ✗ | **✓ `wormhole ssh`** |
| Library / protocol ecosystem | ✗ (app only) | ✗ (app only) | **✓ (importable, Dilation)** |
| Self-host the server / relay | ✓ | ✓ | ✓ |

## Privacy: what a server can observe

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| File contents | ✗ never | ✗ never | ✗ never |
| File names / sizes / hashes | ✗ never | ✗ never | ✗ never (offers encrypted) |
| Ciphertext in the data path | **Only on relay fallback** | Always (relay) | On every NAT-to-NAT transfer (transit relay) |
| The share code | **✗ never (argon2id slot only)** | Room = SHA-256 of code prefix | Words never; nameplate (non-secret) yes |
| Your IP | **Briefly, in RAM, for pairing** | Relay sees peer IP:port | Mailbox + transit see client IPs |
| Persistent logs by default | **None (lifecycle events only)** | Debug-level metadata only | (server is a separate project) |

One thing the data-path tables above don't capture: magic-wormhole's
mailbox is in the loop for *every* transfer's setup — including same-LAN
— whereas fsend's and croc's servers can be entirely offline on a LAN.

## Runtime and distribution

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Language | Go | Go | Python |
| Distribution | **Single static binary** | **Single static binary** | Python package (pip / OS repos) |
| Runtime needed | **None** | **None** | Python ≥ 3.10 + deps (Twisted, etc.) |
| Platforms | Linux, macOS, FreeBSD, Windows; x86 & ARM | Windows, Linux, macOS, FreeBSD, Android/Termux | Anywhere Python + deps run |
| Self-hostable server | ✓ (same binary, `fsend server`) | ✓ (`croc relay`) | ✓ (mailbox + transit, separate projects) |

fsend and croc both drop onto a bare or locked-down machine as one file.
magic-wormhole is convenient where Python and a package manager already
exist (and it's widely packaged in OS repos), but it carries the usual
Python-environment friction and a real dependency stack.

## When each tool is the better fit

**Choose croc if** you depend on its `--qrcode`, `--socks5`,
`--throttleUpload`, `--curve`, or `--git`-aware excludes; you're on a
network that blocks UDP but allows TCP/9009; or you already self-host a
croc relay.

**Choose magic-wormhole if** you need Tor / location privacy; you want to
build on the protocol (the Python library, Dilation, or `wormhole ssh`);
you like choosing your own code phrase or having the receiver allocate
it; or Python is already your path of least resistance.

**Choose fsend if**

- your transfers **cross the internet and throughput matters**: fsend
  hole-punches to a direct path at your own bandwidth; the other two
  relay through a third party;
- you transfer on a **LAN with no internet**: fsend pairs over mDNS with
  the server entirely offline; magic-wormhole can't pair at all;
- you want **harder-to-brute-force codes by default** (rate-limited,
  bounded TTL, code never sent to the server) without asking anyone to
  lengthen anything;
- you want **defense-in-depth** (SPAKE2 *over* TLS 1.3) and
  **post-quantum** forward secrecy — neither of the others has either;
- you want **resume**, a separate **`--pass`** secret, **`--dry-run`**,
  **multi-file** transfers, or **binary stdin/stdout** streaming;
- you'd rather deploy **one static binary** than a Python environment.

---

*Sources: fsend — this repository ([architecture](architecture.md),
[security](security.md), [usage](usage.md)); croc —
`github.com/schollz/croc` v10.4.4; magic-wormhole —
`github.com/magic-wormhole/magic-wormhole` v0.24.0. All rows verified
against current source as of June 2026.*
