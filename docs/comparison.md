# fsend vs croc vs magic-wormhole

A complete, side-by-side comparison of the three leading code-based,
end-to-end-encrypted CLI file-transfer tools.

Every claim below was verified against the **actual source code** or
measured release artifacts, not marketing — fsend at this repo, croc at
[`schollz/croc`](https://github.com/schollz/croc) v11.5.4, and
magic-wormhole at
[`magic-wormhole/magic-wormhole`](https://github.com/magic-wormhole/magic-wormhole)
v0.24.0. Where a tool's README and its code disagree, the code wins and
the disagreement is noted in [Source notes](#source-notes).

All three share the same surface: share a short code, transfer a file,
no accounts, end-to-end encrypted, self-hostable. The differences are in
**how the bytes actually move**, **how strong the encryption and codes
are**, and **what the day-to-day feature set covers**.

> **Last verified October 2026** against fsend v1.15.1 (this repo), croc
> v11.5.4 (`15a4577`), and magic-wormhole 0.24.0 (`3bf46e0`). If any row
> is out of date, please
> [open an issue](https://github.com/polius/fsend/issues) and it'll be
> corrected.

## TL;DR

- **Both fsend and croc can go direct across the internet.** fsend tries
  ICE on every cross-network transfer. croc's default `auto` uses Tailcat —
  Tailscale's userspace WireGuard plus NAT traversal — only for transfers
  of **128 MiB or more**; smaller croc transfers still go through the
  croc relay. magic-wormhole has no hole-punching at all.
- **Strongest cryptography.** fsend is the only one with post-quantum
  hybrid key exchange and two independent layers (TLS 1.3 *plus* SPAKE2
  bound to it). croc's Tailcat path also runs WireGuard, but its node keys
  are derived from the same PAKE session key, so it is not an independent
  trust root. magic-wormhole rests on one SPAKE2-derived key.
- **Codes: fsend is the hardest by default.** fsend codes carry ~45 bits
  and never reach the server (only an Argon2id slot does). croc's default
  is three EFF words — ~31 bits total — and the relay sees a hash of
  the first word, so ~21 bits guard the PAKE. croc's relay does cap joins
  at 30/min per IP and per room by default. magic-wormhole defaults to
  16 bits with no rate limiting.
- **Works on a LAN with no internet.** fsend (mDNS) and croc (multicast
  plus a local relay) pair and transfer offline. magic-wormhole can't —
  it must reach its mailbox server to pair, even on the same Wi-Fi.
- **Different feature centers.** croc has a browser client,
  asynchronous encrypted storage, `croc ssh`, QR codes, SOCKS5, and
  throttling. fsend has `--password`, resume, preview/manifest, JSON
  output, and `fsend doctor`. magic-wormhole has Tor, a library/Dilation,
  and `wormhole ssh`.

## At a glance

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| End-to-end encrypted | ✓ | ✓ | ✓ |
| Cross-network data path | **Direct P2P** via ICE; relay only on fallback | **Tailcat direct** for ≥128 MiB in `auto`; croc relay for smaller/fallback | Direct TCP if a peer is reachable; else transit relay |
| NAT hole-punching | **✓ ICE/STUN** | **✓ Tailscale magicsock** (≥128 MiB in `auto`; `--transport derp` forces) | ✗ |
| Cross-network speed cap | **Your two links** when direct; relay on fallback | **Your links** on Tailcat direct; relay/DERP otherwise; small transfers always relay in `auto` | Relay's bandwidth on NAT-to-NAT |
| LAN without internet | **✓ mDNS, server offline** | **✓ multicast + local relay, server offline** | ✗ (mailbox required to pair) |
| Transport (bulk) | **QUIC + TLS 1.3** over UDP | **WireGuard** over UDP (Tailcat), or TCP + AES-256-GCM (relay) | TCP + NaCl secretbox |
| Servers involved | **One** (pairing; relay only on fallback) | croc relay (always for pairing; data for small/fallback) + Tailscale DERP for Tailcat | Two (mailbox + transit relay) |
| Firewall footprint | **TCP/443 signaling + UDP/443 data/STUN**; ephemeral UDP when direct | **TCP/9009–9017** (public; default self-host 9009–9013) + UDP/TCP 443 | wss/443 + TCP/4001 |
| Defense-in-depth layers | **Two independent** (TLS 1.3 + SPAKE2, channel-bound) | One PAKE root; WireGuard on Tailcat uses the same root | One (secretbox from SPAKE2) |
| MITM protection | **Automatic** (channel-bound) | PAKE only (v11 binds identities and transcript) | SPAKE2 + optional manual `--verify` |
| Post-quantum forward secrecy | **✓ X25519 + ML-KEM-768** | ✗ | ✗ |
| Default code entropy | **~45 bits** | **~31 bits** (~21 after the room word is known) | 16 bits (adjustable) |
| Online-guess protection | **✓ per-IP rate limit + Argon2id slot + bounded TTL** (public on; self-host opt-in) | ✓ relay admission caps: 30 joins/min per IP and per room (default) | None |
| Password on top of the code | **✓ `--password`** | ✗ (`--pass` is relay access, not a transfer secret) | ✗ (the code is the secret) |
| Choose your own code phrase | ✗ | **✓ `--code`** (min 6 chars) | **✓ `--code`** |
| Resume after interruption | **✓ BLAKE3 chunk-verified** | **✓ chunk-based, with reconnect** | ✗ (classic transit restarts) |
| Skip unchanged files on re-send | **✓ size+mtime; `--checksum` to hash** | **✓ hashes (xxhash default)** | ✗ |
| Multiple files per transfer | **✓** | **✓** | ✗ (one file/dir per code) |
| Distribution | **Single static binary** (15.5 MB) | **Single static binary** (23.1 MB; plus separate `croc-web`) | Python package (2.2 MB; ~75 MB installed with deps) |
| License | MIT | MIT (bundled Tailcat is BSD-3) | MIT |

## How the bytes move

Most real transfers cross the internet, between two machines that are
each behind a home or office router (NAT). Neither can see the other
directly, so something has to introduce them. What happens *after* the
introduction is where the three tools differ.

### fsend — direct first, relay only as a true fallback

The pairing server is a matchmaker: both peers register, swap
NAT-traversal candidates through it (ICE), and as soon as one
hole-punched path works, **bytes flow straight from sender to receiver**
at whatever the two links support. The server never sees a file byte. It
falls back to forwarding encrypted UDP (which it can't decrypt) only when
NAT topology defeats hole-punching — a hard symmetric NAT, or a
locked-down corporate network.

```
   Sender ── intro ──►  Pairing server  ◄── intro ── Receiver
      │                 (HTTPS signaling)                  │
      └════════════════ direct P2P ════════════════════════┘
              (NAT hole-punched via ICE; server steps aside)
```

The introduction is ordinary HTTPS over TCP/443. The QUIC data path is
separate and uses UDP.

### croc — relay for pairing, direct for large transfers

For a cross-network transfer, croc's control connection is still a TCP
connection to a croc relay. The relay assigns the room derived from the
code and carries the PAKE messages. After PAKE, the sender's
`--transport` setting decides:

- **`auto` (default), under 128 MiB:** croc opens parallel TCP data
  connections to the relay (default port range 9009–9013), encrypted
  with croc's AES-256-GCM.
- **`auto` (default), 128 MiB or more:** both native peers derive
  Tailcat node identities from the PAKE session key and open TCP streams
  over an in-process Tailscale userspace WireGuard tunnel (eight by
  default). Tailcat bootstraps through a public DERP relay, then
  magicsock tries to upgrade to a **direct UDP path**; DERP remains the
  fallback. croc's AES-256-GCM is still applied inside the tunnel.
- **`derp`:** force the Tailcat path for any size. Despite the name, this
  means direct UDP with DERP as fallback; it requires a Tailcat-capable
  peer and disables croc-relay data fallback.
- **`relay`:** force the old croc-relay data path.

If Tailcat setup fails in `auto`, both sides fall back to the croc relay.
Browsers and older clients use the relay. The relay is always used for
pairing and carries every transfer under 128 MiB in `auto`, but large
native transfers can avoid it.

```
   Sender ── control/PAKE ──► croc relay ◄── control/PAKE ── Receiver
      │                      (TCP 9009)                      │
      ├══ Tailcat direct UDP (≥128 MiB, or --transport derp) ══┤
      └── croc relay data (small / fallback) ─────────────────┘
```

Tailcat is Tailscale's WireGuard data plane without a Tailscale account
or coordination server. The public DERP relays it uses are best-effort
and may apply fairness limits.

### magic-wormhole — direct only if a peer is already reachable

magic-wormhole's transit layer tries a direct TCP connection using
"hints" (each side's local IPs plus a random listening port). This works
when a peer is *already reachable* — same LAN, a public IP, or a manually
forwarded port — but there is **no hole-punching**. So two peers behind
ordinary NATs fall back, after a few seconds, to the transit relay
`transit.magic-wormhole.io:4001`.

```
   Sender ════════►  Transit relay (TCP 4001)  ════════►  Receiver
              when both peers are behind NAT
```

Relaying costs throughput and puts a third party in the data path. The
difference is frequency: **fsend only on hole-punch failure; croc for
every transfer under 128 MiB in `auto`, plus fallback (including
Tailscale DERP if direct fails); magic-wormhole on every NAT-to-NAT
transfer.**

## Same network (both on the same Wi-Fi)

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Local discovery | **mDNS (Pion)** | **IPv4/IPv6 multicast (`239.255.255.250`)** | None |
| Bytes go directly over the LAN | ✓ | ✓ (sender-hosted local relay) | ✓ (transit hints) |
| Pairing works with the server/internet **offline** | **✓** | **✓ (multicast)** | ✗ — mailbox still required |

fsend and croc both discover the peer locally and transfer at link speed
with no internet. **magic-wormhole has no local discovery**: even on the
same Wi-Fi, the PAKE handshake still goes through the central mailbox
server over the internet — only the bulk transit connection is then made
directly. If your two machines share a LAN but have no internet (an
air-gapped office, a field deployment, a plane), fsend and croc work and
magic-wormhole does not.

Croc's local mode uses a relay process on the sender, but it stays on the
LAN; it is not the public relay.

## Transport and ports

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Bulk transport | QUIC over UDP, TLS 1.3 | Tailcat WireGuard over UDP (large), or TCP (relay) | Raw TCP |
| Stream encryption | TLS 1.3 (AEAD) | AES-256-GCM (plus WireGuard on Tailcat) | NaCl secretbox (XSalsa20-Poly1305) |
| Rendezvous channel | HTTPS JSON long-poll over TCP/443, plus ICE/STUN over UDP/443 | TCP control connection to a croc relay; DERP map over HTTPS for Tailcat | WebSocket over TLS (wss/443) |
| Default port(s) | **TCP/443 + UDP/443**; ephemeral UDP when direct | **TCP/9009–9017** public relays (default self-host 9009–9013); UDP + TCP/443 for Tailcat/DERP | wss/443 + TCP/4001 |
| Parallelism | Two QUIC streams (control + data) over one socket | Eight Tailcat streams over one tunnel; multiple TCP data connections on the relay | Single transit connection |
| Firewall footprint | **TCP/443 + UDP/443**; one data port | **TCP/9009–9017** plus UDP/TCP 443 | Two ports (443 + 4001) |
| IPv6 | **✓** | **✓ (IPv6-first, IPv4 fallback)** | Best-effort (not first-class) |

fsend's signaling is **HTTPS over TCP/443**, not HTTP/3. UDP/443 is the
QUIC data/STUN port; it shares a port number with HTTP/3 but carries
fsend's own QUIC protocol. On a direct path, both peers use ephemeral UDP
ports, so the fixed footprint is just the two 443s.

croc's 9009–9017 range and magic-wormhole's TCP/4001 are non-standard
ports that corporate egress filters frequently block. croc can still use
Tailscale DERP over TCP/443 for a large transfer if the control relay is
reachable, and `--transport relay` keeps everything on the croc relay.
magic-wormhole's wss/443 handshake can succeed while the relayed transfer
on 4001 fails to connect. (croc offers `--socks5`; magic-wormhole offers
Tor; fsend has no proxy mode.)

## Cryptography

All three are end-to-end encrypted: an attacker on the wire — or
operating the server — cannot read your file. The strength of that
guarantee is where they separate.

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| End-to-end encrypted | ✓ | ✓ | ✓ |
| PAKE protocol | SPAKE2 (`gospake2`, symmetric construction) | `schollz/pake` v3 (SPAKE2, identity- and transcript-bound in v11) | SPAKE2 (`python-spake2`, symmetric mode) |
| Defense-in-depth layers | **Two independent** — TLS 1.3 (QUIC) **+** SPAKE2 channel-bound to the TLS handshake (RFC 5705) | One PAKE root — AES-256-GCM; on Tailcat, WireGuard node keys are also derived from the PAKE session key | One — secretbox keyed from SPAKE2 |
| AEAD cipher | TLS 1.3 ciphers (AES-GCM / ChaCha20-Poly1305) | AES-256-GCM | NaCl secretbox (XSalsa20-Poly1305) |
| Key derivation | TLS 1.3 KDF + SPAKE2 + RFC 5705 exporter binding | SPAKE2 → HKDF-SHA256 (bound to participants, room, curve, and transcript) → AES-256-GCM | HKDF-SHA256 from the SPAKE2 key |
| Post-quantum forward secrecy | **✓ X25519 + ML-KEM-768 (NIST FIPS 203)** | ✗ classical ECC only | ✗ classical only |
| MITM defense | TLS catches a network MITM; SPAKE2 binding catches a TLS-handshake MITM — **automatic** | Single PAKE layer; v11 binds identities/transcript, but there is no out-of-band verifier | SPAKE2 + optional manual `--verify` string |
| Fresh keys per transfer | ✓ ephemeral Ed25519 cert (1 h) and fresh TLS/PAKE session | PAKE session key per transfer; Tailcat identities derived per session | SPAKE2 session key per transfer |
| Integrity check | **BLAKE3 per-chunk + BLAKE3 root over the whole file** | per-message GCM tag; file hashes (xxhash default) for resume/skip | per-record Poly1305 MAC **+ SHA-256 whole-file check in the close ack** |
| Relay can decrypt | ✗ (relay path only) | ✗ | ✗ |

**Two layers vs one.** A network attacker against fsend must defeat
**both** TLS 1.3 *and* SPAKE2 — and because SPAKE2 is bound to the TLS
handshake via the RFC 5705 exporter, swapping in a forged certificate
breaks the binding and is caught automatically before any file data
flows. croc and magic-wormhole each rest on a single PAKE-derived key.
croc's Tailcat path does encrypt the stream again with WireGuard, but the
WireGuard node keys come from that same PAKE session, so a PAKE
compromise compromises both. It is defense against a broken transport,
not an independent authentication root.

**Post-quantum.** Only fsend's key exchange is quantum-resistant.
Ciphertext recorded today off a croc or magic-wormhole transfer becomes
decryptable once a sufficiently large quantum computer exists; fsend's
X25519 + ML-KEM-768 hybrid is not. croc's Tailcat WireGuard uses
classical X25519.

## Share codes

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Format | `abc-defg-jkm` (3-4-3 letters) | `word-word-word` (three EFF words) | `7-crossover-clockwork` (nameplate + words) |
| Alphabet / wordlist | 23 letters (excludes `i`,`l`,`o`) | 1,296-word EFF Short Wordlist #1 | 256-word PGP word list |
| Default secret entropy | **~45 bits** | **~31 bits** (~21 bits after the room word is known) | 16 bits (two words) |
| Adjustable length | Fixed | Fixed default; custom phrase via `--code` | **`--code-length N`** (~8 bits/word; receiver needs `--allocate`) |
| Custom code phrase | ✗ (system-generated) | **✓ `--code` (min 6 chars)** | **✓ `--code`** |
| Receiver allocates the code | ✗ | ✗ | **✓ `--allocate`** |
| One-shot | ✓ (can't be reused) | ✓ per send (reusable via `--code`) | ✓ fresh code per transfer |
| Server-side TTL | **1 h unclaimed / 10 min after pairing** | 3 h room TTL | ~11 min idle in the reference mailbox server (configurable) |
| Rate limiting | **✓ built-in, per source IP** (public on; self-host opt-in via env) | **✓ relay admission caps: 30 joins/min per IP and per room** (default; configurable) | None (acknowledged in docs) |
| Code reaches a server | **Never** — only an Argon2id-stretched slot | The relay sees a hash of the room word; the PAKE words never | Words never; the (non-secret) nameplate does |

fsend is strong by default, with nothing to configure. The code carries
~45 bits and never reaches the server (both peers register under a 64-MiB
Argon2id-stretched *slot* derived from it), and the server has per-IP
rate limiting built in — on for the public server, opt-in via env when
self-hosting — so online brute force against the full code space is
infeasible.

croc's three-word default is a real trade-off: it is easier to
say than fsend's ten-character code, but the room is
`SHA-256(first word + "croc")`, and a 1,296-word list can be enumerated
offline. The relay can recover the first word; the last two words
(~21 bits) are what the PAKE actually protects. croc's relay admission
caps turn a full online search against a known room into weeks rather
than seconds, but the margin is far smaller than fsend's ~45 bits. A
custom `--code` can be longer if the user chooses one.

magic-wormhole's own
[threat model](https://magic-wormhole.readthedocs.io/en/latest/attacks.html)
is candid about the cost of its 16-bit default: an attacker on the
network or mailbox gets a **1-in-65,536** chance per guess, with no rate
limiting. `--code-length` (on send) and `--allocate` (on receive) let
users lengthen it.

The trade-off is flexibility: magic-wormhole and croc let you choose your
own code phrase, while fsend always picks it for you.

## Features

### Moving data

| Capability | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Send files and folders | ✓ | ✓ (`--zip` optional) | ✓ (folders zipped) |
| Multiple files in one transfer | **✓** | **✓** | ✗ (one file/dir per code) |
| Symlinks | **Followed to content** (broken/cyclic links → error) | **Preserved as links** (target validated) | Followed to content |
| Send from stdin | **✓ (binary)** | **✓** | ✗ (`--text -` only) |
| Stream payload to stdout | **✓ `--out -`** | **✓ (redirect stdout)** | ✗ (text prints; files saved) |
| Send a literal string | `--text` | `--text` | `--text` |
| Receiver confirmation prompt | ✓ (`--yes` to skip) | ✓ (`--yes`) | ✓ (`--accept-file`) |
| Resume after interruption | **✓ BLAKE3-verified** | **✓ chunk-based, with reconnect** | ✗ (classic transit restarts) |
| Skip unchanged files on re-send | **✓ size+mtime; `--checksum`** | **✓ hashes (xxhash default)** | ✗ |
| Preview what you'd send | **✓ `--preview`** — CSV of every file + size, locally, without sending | ✗ | ✗ |
| Record of what was received | **✓ `--manifest`** — CSV of path, size, outcome, SHA-256 | ✗ | ✗ |
| Password on top of the code | **✓ `--password`** | ✗ (`--pass` is relay access) | ✗ (the code is the secret) |
| Exclude paths in a directory | **✓ glob `--exclude`** | **✓ substring `--exclude`, exact `--exclude-file`, `--git`** | ✗ |
| Custom output dir / name | `--out` | `--out`, `--rename`, `--overwrite` | `--output-file` |
| Compression | zstd (automatic; skips chunks it can't shrink) | DEFLATE per-file (`--no-compress` to disable) | DEFLATE (folders only) |
| Bandwidth throttle | ✗ | **✓ `--throttleUpload`** | ✗ |
| QR code for the code | ✗ | **✓ `--qr`** (opens the web receive page) | **✓ (on by default)** |
| SOCKS5 / Tor | ✗ | **✓ `--socks5`** | **✓ `--tor` / `--launch-tor`** |
| Custom elliptic curve | n/a (TLS 1.3) | **✓ `--curve` (P-256 default)** | ✗ |

### Operations and ecosystem

| Capability | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Browser client | ✗ | **✓ getcroc.com (send/receive)** | ✗ |
| Encrypted store-and-forward | ✗ | **✓ `croc store`** — client-side encrypted, expiry, download limit, browser link, revoke | ✗ |
| SSH integration | ✗ | **✓ `croc ssh`** — shared terminal, six-word invitations | **✓ `wormhole ssh`** — adds a public key to `authorized_keys` |
| Machine-readable output | **✓ NDJSON `--json`** | ✗ | ✗ |
| Connectivity diagnostics | **✓ `fsend doctor`** | ✗ | ✗ |
| Self-update | **✓ `fsend --update`** | **✓ `croc update`** | Package manager |
| Library / protocol ecosystem | ✗ (app only) | ✗ (app plus separate web client) | **✓ importable Python library, Dilation** |
| Self-host the server / relay | **✓ same binary (`fsend server`)** | **✓ `croc relay`; `croc-web` for the store** | **✓ separate mailbox + transit projects** |
| Server health / metrics | **✓ `/health` + aggregate `/metrics`** (load, relay bytes, cap hits, rejections — no per-user data) | Web `/healthz` only (no relay metrics) | (server is a separate project) |

croc's store is not a live transfer: the sender uploads ciphertext to
`getcroc.com` (or a self-hosted `croc-web`) and shares a capability link.
The decryption key sits after `#` in the URL fragment and is never sent
to the storage service. The default lifetime is one day and the default
download limit is one.

`croc ssh` is also not a file transfer: it shares a terminal over a
Tailcat WireGuard path with an ephemeral SSH host key, and falls back to
the croc relay if Tailcat is unavailable. It is available on Linux,
macOS, FreeBSD, and OpenBSD. magic-wormhole's `wormhole ssh` solves a
different problem: it sends your public key to a remote
`authorized_keys`.

## Privacy: what a server can observe

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| File contents | ✗ never | ✗ never | ✗ never |
| File names / sizes / hashes | ✗ never | ✗ never | ✗ never (offers encrypted) |
| Ciphertext in the data path | **Only on relay fallback** | Control always; data on the relay for small/fallback; Tailscale DERP sees WireGuard ciphertext if direct fails | On every NAT-to-NAT transfer (transit relay) |
| The share code | **✗ never (Argon2id slot only)** | Room hash of the first word; PAKE words never | Words never; nameplate (non-secret) yes |
| Your IP | **Briefly, in RAM, for pairing** | Relay sees peer IP; Tailscale DERP sees client IP | Mailbox + transit see client IPs |
| Persistent logs by default | **No per-transfer logs (lifecycle events only)** | Debug-level metadata on the relay | (server is a separate project) |

One thing the data-path tables above don't capture: magic-wormhole's
mailbox is in the loop for *every* transfer's setup — including same-LAN
— whereas fsend's and croc's servers can be entirely offline on a LAN.
For croc's large-file path, self-hosting the croc relay does **not**
remove the Tailscale DERP dependency; `--transport relay` is the way to
keep a large transfer off Tailscale infrastructure.

## Runtime and distribution

| | **fsend** | **croc** | **magic-wormhole** |
|---|---|---|---|
| Language | Go | Go | Python |
| Distribution | **Single static binary** | **Single static binary** (plus separate `croc-web`) | Python package (pip / OS repos) |
| Download / installed size | **15.5 MB** binary (**6.1 MB** `.tar.gz`) | **23.1 MB** binary (**9.1 MB** `.tar.gz`) | **214 KB** wheel; **2.2 MB** package; **~75 MB** with dependencies |
| Runtime needed | **None** | **None** | Python ≥ 3.10 + deps (Twisted, etc.) |
| Platforms | Linux, macOS, FreeBSD, Windows; x86 & ARM | Windows, Linux, macOS, FreeBSD, OpenBSD, Android/Termux | Anywhere Python + deps run |
| Self-hostable server | ✓ (same binary, `fsend server`) | ✓ (`croc relay`; `croc-web` for the store) | ✓ (mailbox + transit, separate projects) |

Sizes are Linux amd64 release artifacts for fsend and croc; magic-wormhole
is its wheel plus dependencies resolved in a clean Python 3.14 venv
(excluding pip). fsend is about two-thirds the size of croc's binary.
magic-wormhole has no single binary: its own package is small, but a
working install is dominated by the Python dependency stack.

fsend and croc both drop onto a bare or locked-down machine as one file.
magic-wormhole is convenient where Python and a package manager already
exist (and it's widely packaged in OS repos), but it carries the usual
Python-environment friction and a real dependency stack.

## When each tool is the better fit

**Choose croc if** you want the broadest convenience set: a no-install
browser client, encrypted store-and-forward, `croc ssh`, custom codes, QR
codes, SOCKS5, throttling, or `.gitignore`-aware excludes. It is also the
right pick when you need a large direct transfer and can live with the
Tailscale/DERP path or a TCP-only relay fallback. Be aware that transfers
under 128 MiB use the croc relay, the default code has a smaller security
margin than fsend's, and self-hosting the relay does not remove Tailscale
DERP for large transfers unless you force `--transport relay`.

**Choose magic-wormhole if** you need Tor / location privacy; you want to
build on the protocol (the Python library, Dilation, or `wormhole ssh`);
you like choosing your own code phrase or having the receiver allocate
it; or Python is already your path of least resistance. Its 16-bit
default is the weakest of the three, so lengthen the code or use
`--verify` when the threat model warrants it.

**Choose fsend if**

- you want the **strongest default security margin**: ~45-bit codes that
  never reach the server, Argon2id slot lookup, per-IP rate limiting on
  the public server, and a bounded TTL;
- you want **two independent encryption/authentication layers** and
  **post-quantum** forward secrecy — neither of the others has either;
- your transfers **cross the internet and throughput matters**: fsend
  tries a direct ICE path for every transfer, not only large ones;
- you transfer on a **LAN with no internet**: fsend pairs over mDNS with
  the server entirely offline;
- you want **resume**, a separate **`--password`** secret, a **`--preview`**
  of what you'd send, **multi-file** transfers, or **binary stdin/stdout**
  streaming;
- you want **JSON output**, **`fsend doctor`**, or a **single binary** with
  a self-hosted pairing server and no third-party relay dependency.

---

## Source notes

- **fsend** — this repository, v1.15.1 (`68b5200`): [architecture](architecture.md),
  [security](security.md), [usage](usage.md), and the server/QUIC/ICE
  source under `internal/` and `cmd/`.
- **croc** — [`schollz/croc`](https://github.com/schollz/croc) v11.5.4
  (`15a4577`). Key files: `src/codephrase/codephrase.go`,
  `src/croc/tailcat_negotiation.go`, `src/tcp/defaults.go`,
  `src/publicrelay/publicrelay.go`, `src/pakekey/pakekey.go`.
- **magic-wormhole** — [`magic-wormhole/magic-wormhole`](https://github.com/magic-wormhole/magic-wormhole)
  0.24.0 (`3bf46e0`); the CLI rows were checked at the release tag.
- **Sizes.** fsend from `fsend_1.15.1_linux_amd64.tar.gz`; croc from
  `croc_v11.5.4_Linux-64bit.tar.gz`; magic-wormhole from the 0.24.0
  wheel plus dependencies resolved in a clean Python 3.14 venv.
- **README/source disagreements.** croc's README says the custom code
  maps to an "ordered three-relay pool", while v11.5.4's
  `src/publicrelay` lists four (`1.getcroc.com`–`4.getcroc.com`). croc's
  README also does not mention the 128 MiB `auto` threshold; the source
  does. The public croc deployment publishes TCP 9009–9017, while a
  default `croc relay` uses 9009–9013.

*All rows verified against current source as of October 2026.*
