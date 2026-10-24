# fsend vs croc vs magic-wormhole vs sendme

A side-by-side comparison of four end-to-end-encrypted CLI file-transfer
tools: three that pair with a short code (fsend, croc, magic-wormhole)
and one that pairs with a long ticket (sendme, built on iroh).

Claims were checked against **source code** and release artifacts, not
marketing — fsend at this repo, croc at
[`schollz/croc`](https://github.com/schollz/croc) v11.5.5,
magic-wormhole at
[`magic-wormhole/magic-wormhole`](https://github.com/magic-wormhole/magic-wormhole)
0.24.0, and sendme at
[`n0-computer/sendme`](https://github.com/n0-computer/sendme) v0.37.1.
Where a tool's README and its code disagree, the code wins and the
disagreement is noted in [Source notes](#source-notes). This page is
maintained by fsend's author, so read it with that in mind; rows where
fsend is weaker are kept on purpose.

All four transfer files end to end encrypted, without accounts, and can
be self-hosted. The differences are in **how the bytes actually move**,
**how peers find and authenticate each other**, and **what the feature
set covers**.

> **Last verified 24 October 2026** against fsend's current source
> (wire protocol v3, after v1.15.1), croc v11.5.5 (`8a806c3`),
> magic-wormhole 0.24.0 (`3bf46e0`), and sendme v0.37.1 (`21881bd`). If
> any row is out of date, please
> [open an issue](https://github.com/polius/fsend/issues) and it'll be
> corrected.

## TL;DR

- **Data path.** fsend tries a direct ICE path on every cross-network
  transfer and relays only when that fails. sendme (iroh) also
  hole-punches, with n0's relays as fallback. croc's default `auto`
  uses Tailcat — Tailscale's userspace WireGuard plus NAT traversal —
  only for transfers of **128 MiB or more**; smaller croc transfers go
  through the croc relay. magic-wormhole has no hole-punching.
- **Authentication.** fsend, croc and magic-wormhole authenticate the
  peer with a PAKE over a short code. fsend's is CPace, bound to the TLS
  1.3 session it authenticates. sendme has no PAKE: its ticket names the
  sender's public key, and anyone holding the ticket can download.
- **Post-quantum.** Only fsend's peer key exchange is PQ-hybrid
  (X25519MLKEM768), and it's required. The others use classical
  key exchange.
- **Codes.** fsend codes carry ~45 bits and the server sees only a daily
  salted argon2id slot. croc's default three words carry ~31 bits, and
  the relay can learn the first word. magic-wormhole defaults to 16 bits.
  sendme tickets are long capabilities you copy and paste, not codes.
- **Receivers.** fsend, croc (live sends) and magic-wormhole serve
  exactly one receiver per code; fsend does this by design. sendme
  serves any number of receivers while the sender runs, and croc's
  `store` mode allows several downloads.
- **Same LAN, no internet.** fsend (mDNS) and croc (multicast) pair and
  transfer with no internet. magic-wormhole can't (it needs its mailbox).
  sendme has no local discovery; a ticket with direct addresses can
  still work on a LAN if relays are disabled.
- **Locked-down networks.** fsend falls back to its relay over a
  WebSocket on TCP 443 when UDP is blocked. croc uses TCP 9009+ (and
  DERP over 443 for large transfers); magic-wormhole 0.24.0 uses TCP 4000
  and 4001; sendme's relays speak HTTPS on 443.
- **Where fsend falls short.** No QR code, no browser receiver, no
  multi-receiver sends, no custom code phrases, no SOCKS5/Tor, no
  throttling, no store-and-forward, no library, and no way to pair across
  networks without a pairing server.

## At a glance

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| End-to-end encrypted | ✓ | ✓ | ✓ | ✓ |
| How the receiver finds the sender | Short code (~45 bits) | Short code (three words) | Short code (number + two words) | Long ticket (public key + addresses + hash) |
| Peer authentication | **CPace PAKE**, bound to TLS 1.3 | PAKE (`schollz/pake` v3) | SPAKE2 | Sender's Ed25519 key in the ticket; receiver not authenticated |
| Cross-network data path | **Direct P2P** via ICE; relay only on fallback | **Tailcat direct** for ≥128 MiB in `auto`; croc relay for smaller/fallback | Direct TCP if a peer is reachable; else transit relay | Direct via iroh hole-punching; n0 relay on fallback |
| NAT hole-punching | **✓ ICE/STUN** | **✓ Tailscale magicsock** (≥128 MiB in `auto`) | ✗ | **✓ iroh** |
| UDP blocked | **✓ relay over WebSocket on TCP 443** | croc relay on TCP 9009+ | TCP 4000 + 4001 | iroh relay over HTTPS |
| LAN without internet | **✓ mDNS, server offline** | **✓ multicast + local relay** | ✗ (mailbox required to pair) | Only with relays disabled and a direct address in the ticket |
| Pair across networks with no server | ✗ | ✗ | ✗ | ✓ if one side is directly reachable (`--relay disabled`) |
| Receivers per send | **One** (by design) | One (live); N with `croc store` | One | **Many**, while the sender runs |
| Transport (bulk) | QUIC + TLS 1.3 over UDP (or over the TCP fallback) | WireGuard over UDP (Tailcat), or TCP + AES-256-GCM (relay) | TCP + NaCl secretbox | QUIC + TLS 1.3 |
| PQ-hybrid key exchange | **✓ X25519MLKEM768, required** | ✗ | ✗ | ✗ |
| Default secret strength | **~45-bit code** | ~31-bit code (~21 after the room word is known) | 16-bit code (adjustable) | Not a guessable code: a capability that must be copied whole |
| Online-guess protection | **Per-IP rate limit, argon2id slot, 10-failure cap per sender** (rate limit: public on, self-host opt-in) | Relay caps: 30 joins/min per IP and per room | None | n/a (no code to guess) |
| Password on top | **✓ `--password`** (mutual PAKE) | ✗ (`--pass` is relay access) | ✗ | ✗ |
| Choose your own code | ✗ | **✓ `--code`** (min 6 chars) | **✓ `--code`** | ✗ |
| Resume after interruption | **✓** (original receiver device only) | **✓** | ✗ | **✓** |
| Skip unchanged files on re-send | **✓ size+mtime; `--checksum`** | **✓ hashes** | ✗ | ✗ (resume of the same ticket only) |
| Multiple files per transfer | ✓ | ✓ | ✗ (one file/dir per code) | ✓ (one file or directory) |
| QR code | ✗ | **✓ `--qr`** | **✓ (on by default)** | ✗ |
| Browser receiver | ✗ | **✓ getcroc.com** | Third-party (Winden) | ✗ |
| Stdin / stdout | **✓ / ✓** | ✓ / ✓ | `--text -` only / ✗ | ✗ / ✗ |
| Distribution | Single static binary (16.5 MB) | Single static binary (23.7 MB) | Python package (~75 MB installed with deps) | Single binary (11.1 MB) or `cargo install` |
| License | MIT | MIT (bundled Tailcat is BSD-3) | MIT | Apache-2.0 OR MIT |

## How the bytes move

Most real transfers cross the internet, between two machines that are
each behind a home or office router (NAT). Neither can see the other
directly, so something has to introduce them. What happens *after* the
introduction is where the tools differ.

### fsend — direct first, relay only as a fallback

The pairing server is a matchmaker: both peers register under a slot
derived from the code, swap NAT-traversal candidates through it (ICE),
and as soon as one hole-punched path works, **bytes flow straight from
sender to receiver**. The server never sees a file byte. It falls back to
forwarding encrypted QUIC packets (which it can't decrypt) only when NAT
topology defeats hole-punching — a hard symmetric NAT, or a locked-down
network. If the network blocks UDP, the relay is reached over a
WebSocket on the server's HTTPS port instead.

```
   Sender ── intro ──►  Pairing server  ◄── intro ── Receiver
      │                 (HTTPS signaling)                  │
      └════════════════ direct P2P ════════════════════════┘
              (NAT hole-punched via ICE; server steps aside)
```

The introduction is ordinary HTTPS over TCP/443. The QUIC data path is
separate and uses UDP, or the TCP 443 fallback for the relay.

A direct transfer can't beat the slower of the two links, and latency,
loss and CPU lower it further. The relay is limited by the server's
bandwidth and caps.

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
- **`relay`:** force the croc-relay data path.

If Tailcat setup fails in `auto`, both sides fall back to the croc relay.
Browsers and older clients use the relay.

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
forwarded port — but there is **no hole-punching**. Two peers behind
ordinary NATs fall back, after a few seconds, to the transit relay
`transit.magic-wormhole.io:4001`.

```
   Sender ════════►  Transit relay (TCP 4001)  ════════►  Receiver
              when both peers are behind NAT
```

### sendme — iroh: direct, with n0's relays as fallback

sendme prints a **ticket** rather than a code: the sender's iroh endpoint
ID (an Ed25519 public key), its relay URL and direct addresses, and the
hash of the content. The receiver dials that endpoint over QUIC; iroh
tries to hole-punch a direct path and otherwise forwards the encrypted
QUIC traffic through a relay (n0 runs public ones over HTTPS/443, and
`iroh-relay` can be self-hosted). Data is BLAKE3-verified as it streams
(iroh-blobs).

```
   Sender ◄──── QUIC (direct, hole-punched) ────► Receiver
      └──────── or via an iroh relay (HTTPS/443) ──────┘
```

The sender keeps serving until you stop it, and any number of receivers
holding the ticket can fetch.

**How often a relay is in the path:** fsend only on hole-punch failure;
sendme only on hole-punch failure; croc for every transfer under 128 MiB
in `auto`, plus fallback; magic-wormhole on every NAT-to-NAT transfer.

## Same network (both on the same Wi-Fi)

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| Local discovery | **mDNS** | **IPv4/IPv6 multicast (`239.255.255.250`)** | None | None (direct addresses in the ticket) |
| Bytes go directly over the LAN | ✓ | ✓ (sender-hosted local relay) | ✓ (transit hints) | ✓ |
| Pairing works with the internet **offline** | **✓** | **✓** | ✗ — mailbox required | With `--relay disabled` |

fsend and croc both discover the peer locally and transfer at link speed
with no internet. **magic-wormhole has no local discovery**: even on the
same Wi-Fi, the PAKE handshake goes through the central mailbox server —
only the bulk transit connection is then made directly. sendme needs no
discovery because the ticket carries the sender's addresses, but by
default the sender waits to come online with a relay first, so an
offline LAN needs `--relay disabled` (from reading the code; not
tested here).

fsend's mDNS discovery can still fail on networks with client isolation
or a firewall on the listener's port; it then pairs through the server.

## Transport and ports

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| Bulk transport | QUIC, TLS 1.3 | Tailcat WireGuard over UDP (large), or TCP (relay) | Raw TCP | QUIC, TLS 1.3 |
| Stream encryption | TLS 1.3 (AEAD) | AES-256-GCM (plus WireGuard on Tailcat) | NaCl secretbox (XSalsa20-Poly1305) | TLS 1.3 (AEAD) |
| Rendezvous channel | HTTPS JSON long-poll over TCP/443, plus ICE/STUN over UDP/443 | TCP control connection to a croc relay; DERP map over HTTPS for Tailcat | Plain WebSocket, `ws://relay.magic-wormhole.io:4000` | None (the ticket); relay over HTTPS/443 |
| Ports a client needs | **TCP/443 + UDP/443**; works with TCP/443 alone via the relay fallback | TCP/9009–9017 (public relays); UDP + TCP/443 for Tailcat/DERP | TCP/4000 + TCP/4001 | UDP for direct; HTTPS/443 to the relay |
| Parallelism | Two QUIC streams (control + data) | Eight Tailcat streams, or several TCP connections to the relay | Single transit connection | QUIC streams |
| IPv6 | ✓ | ✓ (IPv6-first) | Best-effort | ✓ |

fsend's signaling is HTTPS over TCP/443, not HTTP/3. UDP/443 carries
fsend's own QUIC protocol and STUN. On a network that blocks UDP, fsend
reaches the relay over a WebSocket on the same HTTPS endpoint; what
still fails is a TLS-intercepting proxy whose certificate the system
doesn't trust (`E047`). fsend has no SOCKS5 or Tor mode.

croc's 9009–9017 and magic-wormhole's TCP 4000/4001 are non-standard
ports that corporate egress filters often block. croc can still use
Tailscale DERP over TCP/443 for a large transfer if the control relay is
reachable. magic-wormhole's unreleased master switches the mailbox to
`wss://relay.magic-wormhole.io` on 443 (commit `f0522ac`); the transit
relay stays on 4001. croc offers `--socks5`; magic-wormhole offers Tor.

## Cryptography

All four are end-to-end encrypted: an attacker on the wire — or
operating the server — cannot read your file. The differences are in how
the peers authenticate and what the key exchange resists.

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| PAKE | **CPace** (draft-irtf-cfrg-cpace-21, X25519/SHA-512) | `schollz/pake` v3 (SPAKE2-style, identity- and transcript-bound in v11) | SPAKE2 (`python-spake2`) | None |
| How authentication covers the data | TLS 1.3 session authenticated by CPace through the TLS exporter (channel binding); TLS certificates are not verified | Data keys derived from the PAKE key; on Tailcat, WireGuard keys too | secretbox key derived from SPAKE2 | TLS 1.3 authenticated by the endpoint's public key from the ticket |
| AEAD | TLS 1.3 (AES-GCM / ChaCha20-Poly1305) | AES-256-GCM | XSalsa20-Poly1305 | TLS 1.3 |
| Key exchange | **X25519MLKEM768 (PQ-hybrid), required** | Classical ECC (P-256 default, `--curve`) | Classical | Classical (X25519 / P-256 / P-384) |
| MITM defense | Automatic: a MITM gets a different TLS exporter on each side and fails the confirmation | PAKE; no out-of-band verifier | SPAKE2 + optional manual `--verify` | Ticket integrity: whoever gives you the ticket decides whom you connect to |
| Second secret | `--password`: a second, mutual CPace run bound to the session | ✗ | ✗ | ✗ |
| Integrity check | QUIC AEAD + **per-file BLAKE3 root** (1 MiB blocks) | GCM tags; file hashes for resume/skip | Poly1305 per record + SHA-256 whole-file | BLAKE3 verified streaming |
| Relay can decrypt | ✗ | ✗ | ✗ | ✗ |

**What the channel binding buys.** fsend's TLS layer encrypts but, on its
own, authenticates nothing (the sender's certificate is self-signed and
unverified). The CPace run uses the TLS exporter as its session id and
confirms it, so a man-in-the-middle that terminates TLS on both sides —
including a malicious pairing server or relay — ends up with two
different exporters and fails the check before any data flows. It is one
authenticated channel, not two independent layers. croc and
magic-wormhole likewise derive everything from their PAKE key; croc's
Tailcat path encrypts again with WireGuard, but those keys come from the
same PAKE session.

**Post-quantum.** fsend requires the X25519MLKEM768 hybrid for the
peer-to-peer TLS handshake, so a recording of today's transfer stays
confidential against a future quantum computer. The other three use
classical key exchange, so recorded traffic would become decryptable
once a large enough quantum computer exists. fsend's PAKE itself is
classical (X25519): such an attacker could recover the code from a
recorded handshake, but not the transfer keys.

**Tickets vs codes.** sendme's ticket contains no low-entropy secret to
guess, so it needs no PAKE or rate limiting. In exchange it's a bearer
capability: anyone who sees it can download while the sender runs, and
it's too long to read aloud.

## Share codes

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| Format | `abc-defg-jkm` (3-4-3 letters) | `word-word-word` (three EFF words) | `7-crossover-clockwork` | Base32 ticket, well over 100 characters |
| Default secret entropy | **~45 bits** | ~31 bits (~21 after the room word is known) | 16 bits | n/a (capability) |
| Adjustable / custom | Fixed, system-generated | Custom phrase via `--code` (min 6 chars) | `--code-length N`, `--code`, receiver `--allocate` | Ticket type (`--ticket-type`) |
| Receivers per code | One; resume only from the same device | One per live send | One (a third side gets "crowded") | Unlimited while the sender runs |
| Lifetime | Up to 1 h per pairing round; ends when the sender exits; used codes rejected for 10 min | 3 h room TTL | ~11 min idle on the mailbox | Until the sender stops |
| Rate limiting | ✓ per source IP (public on; self-host opt-in) + 10 failed handshakes per sender | ✓ relay caps: 30 joins/min per IP and per room; 32 pending handshakes per IP | None (acknowledged in docs) | n/a |
| What a server sees of it | An argon2id slot, salted per UTC day | A hash of the first word (the room) | The (non-secret) nameplate | The relay sees endpoint IDs and IPs; it isn't given the ticket |

fsend's server never receives the code. Both peers register under a slot
(argon2id of the code with 64 MiB of memory, salted with the UTC day), so
recovering a code from a slot means a memory-hard search of the ~2^45
code space, and a precomputed table expires daily. Once a receiver has
connected, both peers switch to a key derived from their session, so the
code can't be used to step into a dropped transfer.

croc's three-word default is easier to say than fsend's ten letters, but
the room is `SHA-256(first word + "croc")` and the 1,296-word list can be
enumerated, so the relay can recover the first word; the last two words
(~21 bits) are what the PAKE protects. croc's relay admission caps make a
full online search slow. A custom `--code` can be longer.

magic-wormhole's own
[threat model](https://magic-wormhole.readthedocs.io/en/latest/attacks.html)
is candid about its 16-bit default: an attacker gets a **1-in-65,536**
chance per guess, with no rate limiting. `--code-length` (on send) and
`--allocate` (on receive) lengthen it.

The trade-off cuts the other way too: croc and magic-wormhole let you
choose a code phrase, and sendme's tickets need no server at all to be
secure; fsend always picks the code and needs its pairing server across
networks.

## Features

"—" means the row wasn't checked for that tool.

### Moving data

| Capability | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| Send files and folders | ✓ | ✓ | ✓ (folders zipped) | ✓ (one file or directory) |
| Multiple paths in one transfer | ✓ | ✓ | ✗ | ✗ |
| Several receivers for one send | ✗ (by design) | `croc store --downloads N` | ✗ | ✓ |
| Symlinks | Followed to content | Preserved as links | Followed to content | — (not checked) |
| Send from stdin | ✓ (binary) | ✓ | ✗ (`--text -` only) | ✗ |
| Stream payload to stdout | ✓ `--out -` (folders as tar) | ✓ | ✗ | ✗ |
| Send a literal string | `--text` | `--text` | `--text` | ✗ |
| Receiver confirmation prompt | ✓ (`--yes` to skip) | ✓ (`--yes`) | ✓ (`--accept-file`) | — |
| Resume after interruption | ✓ (prefix verified by BLAKE3) | ✓ | ✗ | ✓ |
| Existing files on the receiver | Skip identical; ask once: keep both / replace / skip | `--overwrite` | `--output-file` | — |
| Preview what you'd send | ✓ `--preview` (CSV) | ✗ | ✗ | ✗ |
| Record of what was received | ✓ `--manifest` (CSV with SHA-256) | ✗ | ✗ | ✗ |
| Exclude paths | ✓ glob `--exclude` | ✓ `--exclude`, `--exclude-file`, `--git` | ✗ | ✗ |
| Compression | zstd (automatic; backs off on incompressible data) | DEFLATE (`--no-compress`) | DEFLATE (folders only) | — |
| Bandwidth throttle | ✗ | ✓ `--throttleUpload` | ✗ | ✗ |
| QR code | ✗ | ✓ `--qr` (web receive page) | ✓ (on by default) | ✗ (`--clipboard` copies the ticket) |
| SOCKS5 / Tor | ✗ | ✓ `--socks5` | ✓ `--tor` | ✗ |

### Operations and ecosystem

| Capability | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| Browser client | ✗ | ✓ getcroc.com | Third-party (Winden) | ✗ |
| Encrypted store-and-forward | ✗ | ✓ `croc store` | ✗ | ✗ |
| Machine-readable output | ✓ NDJSON `--json` | ✗ | ✗ | ✗ |
| Connectivity diagnostics | ✓ `fsend doctor` (server, UDP relay, TCP fallback, mDNS) | ✗ | ✗ | ✗ |
| Self-update | ✓ `fsend --update` (verifies checksums; signature with cosign) | ✓ `croc update` | Package manager | `cargo install` / releases |
| Signed releases | ✓ cosign (keyless) + GitHub attestations + SBOMs | — | PyPI | — |
| Library / protocol ecosystem | ✗ (app only) | ✗ | ✓ Python library; Dilation (in development); Go and Rust ports | ✓ built on iroh, a Rust library |
| Self-host the server / relay | ✓ same binary (`fsend server`) | ✓ `croc relay`; `croc-web` | ✓ separate mailbox + transit projects | ✓ `iroh-relay` |
| Server metrics | ✓ aggregate Prometheus on a private listener | Web `/healthz` only | (separate project) | (iroh-relay) |

**Scope.** croc also ships `croc ssh` (a shared terminal) and
magic-wormhole `wormhole ssh` (installs a public key). fsend
deliberately stays a file-transfer tool and doesn't plan features like
these, or multi-receiver sends, which would weaken the one-code,
one-receiver model.

## Privacy: what a server can observe

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| File contents | ✗ never | ✗ never | ✗ never | ✗ never |
| File names / sizes / hashes | ✗ never | ✗ never | ✗ never | ✗ never (relay sees encrypted QUIC) |
| Ciphertext in the data path | Only on relay fallback | Control always; data on the relay for small/fallback; DERP sees WireGuard ciphertext if direct fails | On every NAT-to-NAT transfer | Only on relay fallback |
| The secret | ✗ never (argon2id slot only) | Room hash of the first word | Nameplate (non-secret) only | The relay isn't given the ticket |
| Your IP | In RAM, for pairing | Relay; Tailscale DERP for Tailcat | Mailbox + transit | Relay |
| Default logs | No per-transfer lines; no IPs or slots | Debug-level metadata | (separate project) | (iroh-relay) |

magic-wormhole's mailbox is in the loop for *every* transfer's setup,
including same-LAN, whereas fsend's and croc's servers can be offline
on a LAN. For croc's large-file path, self-hosting the croc relay does
not remove the Tailscale DERP dependency; `--transport relay` does.

## Runtime and distribution

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---|---|---|---|
| Language | Go | Go | Python | Rust |
| Download / installed size | **16.5 MB** binary (**6.5 MB** `.tar.gz`) | 23.7 MB binary (9.4 MB `.tar.gz`) | 214 KB wheel; ~75 MB with dependencies | 11.1 MB binary (4.9 MB `.tar.gz`) |
| Runtime needed | None | None | Python ≥ 3.10 + deps | None |
| Platforms | Linux, macOS, FreeBSD, Windows; x86 & ARM | Windows, Linux, macOS, FreeBSD, OpenBSD, Android/Termux | Anywhere Python runs | Linux, macOS, Windows |

Sizes are Linux amd64. fsend's is built from the current source with the
release flags (`-trimpath -ldflags "-s -w"`); croc's and sendme's are
their release archives; magic-wormhole's is the 0.24.0 wheel plus
dependencies resolved in a clean Python 3.14 venv.

## When each tool is the better fit

**Choose croc if** you want the broadest convenience set: a browser
client, encrypted store-and-forward with several downloads, custom
codes, QR codes, SOCKS5, throttling, or `.gitignore`-aware excludes. Be
aware that transfers under 128 MiB use the croc relay, the default code
has a smaller margin than fsend's, and self-hosting the relay does not
remove Tailscale DERP for large transfers unless you force
`--transport relay`.

**Choose magic-wormhole if** you need Tor; you want to build on the
protocol (the Python library, Dilation, or other implementations); or you
like choosing your own code phrase or having the receiver allocate it.
Its 16-bit default is the weakest here, so lengthen the code or use
`--verify` when it matters.

**Choose sendme if** you want to hand the same download to several
people, or pair without any rendezvous server, and copying a long ticket
is fine — for example between your own machines, or in scripts. There is
no PAKE, so the ticket must travel over a channel you trust, and there
is no stdin/stdout streaming.

**Choose fsend if**

- you want a **short code that is safe to read aloud**: ~45 bits, never
  sent to the server, one receiver, and a dropped transfer that only the
  original receiver device can resume;
- you want a **PQ-hybrid key exchange** (X25519MLKEM768) required on
  every transfer, and an optional `--password` checked by a second PAKE;
- your transfers **cross the internet**: fsend tries a direct path for
  every transfer, and falls back to a relay over TCP 443 when UDP is
  blocked;
- you transfer on a **LAN with no internet**: fsend pairs over mDNS with
  the server offline;
- you want **resume**, **`--preview`**, a **`--manifest`** with SHA-256,
  **binary stdin/stdout**, **JSON output**, **`fsend doctor`**, or a
  **single binary** that is also the self-hosted server.

---

## Source notes

- **fsend** — this repository, after v1.15.1: [architecture](architecture.md),
  [security](security.md), [usage](usage.md), and the source under
  `internal/` (`pake`, `quicconn`, `code`, `relay`, `server`,
  `transfer`) and `cmd/fsend`.
- **croc** — [`schollz/croc`](https://github.com/schollz/croc)
  [v11.5.5](https://github.com/schollz/croc/releases/tag/v11.5.5)
  (`8a806c3`, 19 October 2026). Key files:
  [`src/codephrase/codephrase.go`](https://github.com/schollz/croc/blob/v11.5.5/src/codephrase/codephrase.go)
  (EFF list, room hash, `--code` length),
  [`src/croc/tailcat_negotiation.go`](https://github.com/schollz/croc/blob/v11.5.5/src/croc/tailcat_negotiation.go)
  (128 MiB threshold),
  [`src/tcp/defaults.go`](https://github.com/schollz/croc/blob/v11.5.5/src/tcp/defaults.go)
  (join caps, pending-handshake caps, room TTL),
  [`src/cli/cli.go`](https://github.com/schollz/croc/blob/v11.5.5/src/cli/cli.go)
  (flags), `src/tcp/tcp.go` (two peers per room), and
  [`disco.json`](https://github.com/schollz/croc/blob/v11.5.5/disco.json)
  (public relay ports).
- **magic-wormhole** — [`magic-wormhole/magic-wormhole`](https://github.com/magic-wormhole/magic-wormhole)
  [0.24.0](https://pypi.org/project/magic-wormhole/0.24.0/) (`3bf46e0`,
  5 May 2026): [`cli/cli.py`](https://github.com/magic-wormhole/magic-wormhole/blob/0.24.0/src/wormhole/cli/cli.py)
  (flags, QR default),
  [`cli/public_relay.py`](https://github.com/magic-wormhole/magic-wormhole/blob/0.24.0/src/wormhole/cli/public_relay.py)
  (mailbox `ws://…:4000`, transit `:4001`), and
  [mailbox-server 0.8.0](https://github.com/magic-wormhole/magic-wormhole-mailbox-server/blob/0.8.0/src/wormhole_mailbox_server/server_tap.py)
  (11-minute channel expiry). Other implementations are listed in
  [`docs/ecosystem.rst`](https://github.com/magic-wormhole/magic-wormhole/blob/0.24.0/docs/ecosystem.rst).
  The "no hole-punching" and "no local discovery" rows carry over from
  the previous audit and weren't re-traced for this one.
- **sendme** — [`n0-computer/sendme`](https://github.com/n0-computer/sendme)
  [v0.37.1](https://github.com/n0-computer/sendme/releases/tag/v0.37.1)
  (`21881bd`, 21 October 2026): [`src/main.rs`](https://github.com/n0-computer/sendme/blob/v0.37.1/src/main.rs)
  (ticket, receivers, resume store, `--relay`),
  [`Cargo.toml`](https://github.com/n0-computer/sendme/blob/v0.37.1/Cargo.toml)
  (iroh 1.x, iroh-blobs, license), and iroh's
  [default relays](https://docs.rs/crate/iroh/1.3.0/source/src/defaults.rs).
  The PQ row reflects iroh's default `tls-ring` feature (rustls' ring
  provider offers X25519, P-256 and P-384). What the relay sees is from
  iroh's design rather than traced in code, and the offline-LAN
  behaviour and ticket length are from reading the code, not tested.
- **Sizes.** fsend built from this repository for linux/amd64; croc from
  `croc_v11.5.5_Linux-64bit.tar.gz`; sendme from its v0.37.1 Linux
  x86_64 release; magic-wormhole from the 0.24.0 wheel plus dependencies.
- **README/source disagreements.** croc's README says the custom code
  maps to an "ordered three-relay pool", while v11.5.5's
  `src/publicrelay` lists four (`1.getcroc.com`–`4.getcroc.com`). croc's
  README doesn't mention the 128 MiB `auto` threshold; the source does.
  The public croc deployment publishes TCP 9009–9017, while a default
  `croc relay` uses 9009–9013.
