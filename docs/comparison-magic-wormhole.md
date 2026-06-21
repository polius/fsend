# fsend vs magic-wormhole

Both tools do the same thing at the surface: share a short code, transfer
a file. They're CLI-first, no accounts, end-to-end encrypted, and both
authenticate the transfer with a SPAKE2 PAKE. If you've used one, the
other will feel familiar.

The differences show up **when the two computers are on different
networks** — the common case — and in two design choices that ripple
through everything else: how the code is sized, and what the transport
is built on.

## TL;DR

- **Across the internet, fsend goes directly between the two computers
  when it can; magic-wormhole only goes direct if one peer is already
  reachable, otherwise it relays.** fsend hole-punches through NATs (ICE);
  magic-wormhole has no hole-punching, so two peers behind ordinary home
  or café routers fall back to its transit relay — every byte through a
  third party.
- **fsend's share code carries ~45 bits of entropy; magic-wormhole's
  default carries 16.** magic-wormhole's own threat model documents the
  consequence: a network or server-side attacker has a 1-in-65536 chance
  of guessing the code.
- **fsend layers SPAKE2 on top of TLS 1.3 and adds post-quantum key
  exchange.** magic-wormhole uses one layer — SPAKE2 → NaCl secretbox —
  with classical crypto only.
- **fsend is a single static binary; magic-wormhole is a Python program**
  (≥ 3.10) installed via pip or your OS package manager.

## Same network: a real difference here

On the same Wi-Fi, both tools end up sending the file bytes directly over
the LAN. But they get there differently, and it matters when the internet
is flaky or absent.

```
   ┌────────┐                                                  ┌──────────┐
   │        │◄─────────── mDNS discovery (fsend) ─────────────►│          │
   │ Sender │                                                  │ Receiver │
   │        │◄═══════════ direct LAN transfer ════════════════►│          │
   └────────┘                                                  └──────────┘
```

**fsend discovers the peer locally over mDNS** — no server, no internet.
On the same LAN, fsend can pair and transfer with the pairing server
completely offline.

**magic-wormhole has no local discovery.** Even when both machines are on
the same Wi-Fi, the PAKE handshake still goes through the central mailbox
server over the internet; only the bulk *transit* connection is then made
directly over the LAN. So same-network transfers still depend on reaching
`relay.magic-wormhole.io` to get started.

If your two machines share a LAN but have no internet (an air-gapped
office, a field deployment, a plane), fsend still works and
magic-wormhole does not.

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

The pairing server introduces the two peers, they swap NAT-traversal
candidates through it, and **as soon as one hole-punched path works, bytes
flow straight from sender to receiver** at whatever the two internet links
support. The server never sees a file byte. Only when NAT topology defeats
hole-punching (hard symmetric NATs, locked-down corporate networks) does
the server fall back to forwarding encrypted UDP it can't decrypt.

### magic-wormhole: direct only if a peer is reachable, else relay

```
   ┌────────┐                  ┌───────────────┐                  ┌──────────┐
   │ Sender │ ════════════════►│ Transit relay │ ════════════════►│ Receiver │
   └────────┘   every byte     │   TCP, 4001   │   every byte     └──────────┘
                               └───────────────┘
                       (default: transit.magic-wormhole.io)
```

magic-wormhole's transit layer tries a direct TCP connection first: each
side lists its local IP addresses, listens on a random TCP port, and
sends those as "hints" to the peer. This succeeds **when a peer is already
reachable** — same LAN, a public IP, or a manually forwarded port. But
there is **no NAT hole-punching** (no STUN/ICE; the docs list
STUN-assisted traversal as a future idea, not a shipped feature). So two
peers behind ordinary NATs — the typical home-to-café case — cannot reach
each other directly, and after a few seconds both fall back to the transit
relay.

On that fallback path every cross-network byte makes **two trips across
the internet** — sender → relay → receiver. Three things follow:

| Consequence        | Why it matters                                                          |
|--------------------|-------------------------------------------------------------------------|
| **Throughput cap** | Bounded by the relay's bandwidth, not your own links.                  |
| **Latency**        | Round-trip is sender → relay → receiver, even when peers are nearby.    |
| **Data path**      | Every cross-network byte (encrypted) flows through a third-party server. |

The relay sees only ciphertext — but it sees it, on every NAT-to-NAT
transfer. fsend's relay sees the same opaque ciphertext, but only on the
rare fallback path; in the common cross-internet case it sees nothing at
all because the peers connected directly.

## Two servers vs one

A subtle structural difference: magic-wormhole uses **two** separate
servers, fsend uses **one**.

|                   | fsend                                  | magic-wormhole                                       |
|-------------------|----------------------------------------|-----------------------------------------------------|
| Rendezvous        | Pairing server (also the relay)        | Mailbox server (`relay.magic-wormhole.io`, wss/443) |
| Relay             | Same pairing server, fallback only     | Separate transit relay (`transit.…:4001`)           |
| Server sees code? | Never — only an argon2id-stretched slot| Never the words — but the SPAKE2 protocol messages and the plaintext nameplate pass through the mailbox |

In magic-wormhole the **mailbox server is in the path of every transfer's
setup**: it relays the SPAKE2 PAKE messages (public protocol values, not
the words) and the end-to-end-encrypted application phases (the offer and
transit hints are sent through `wormhole.send`, so the mailbox sees them
only as ciphertext). Like any server, it also observes each client's
public IP from their WebSocket connection. It never sees file bytes —
those go over the transit layer — but it is always involved, even on the
same LAN. fsend's pairing server does the same matchmaking job, but on the
common cross-internet path it steps aside the moment the direct connection
is up.

## Transport and ports

|                     | fsend                                  | magic-wormhole                                          |
|---------------------|----------------------------------------|--------------------------------------------------------|
| Transport (bulk)    | QUIC over UDP, TLS 1.3                  | Raw TCP, NaCl secretbox (not TLS)                       |
| Rendezvous          | HTTP/3 over UDP/443                     | WebSocket over TLS (wss/443)                            |
| Default port(s)     | UDP/443 (server); ephemeral for direct | wss/443 (mailbox) + TCP/4001 (transit relay)           |
| Direct-path ports   | Hole-punched ephemeral UDP             | Random ephemeral TCP (only reachable peers)            |
| Firewall footprint  | One server-side UDP port               | Outbound 443 **and** TCP/4001                           |
| IPv6                | ✓                                      | ✓                                                      |

Both relays listen on a single port (UDP/443 for fsend, TCP/4001 for
magic-wormhole), so neither uses a multi-port range. The difference is
*which* port. fsend's traffic rides UDP/443 — the port HTTP/3 uses.
magic-wormhole reaches its mailbox over wss/443, but its transit relay
listens on **TCP/4001**, a non-standard port. On a network whose egress
filtering allows 443 but not 4001, the mailbox handshake can complete
while the relayed transfer cannot connect.

## Cryptography

Both tools are end-to-end encrypted and both use SPAKE2 — fsend an
RFC 9382 implementation, magic-wormhole the dedicated `python-spake2`
library. The differences are in the layers built around it.

|                              | fsend                                                                         | magic-wormhole                                                  |
|------------------------------|-------------------------------------------------------------------------------|----------------------------------------------------------------|
| End-to-end encrypted         | ✓                                                                             | ✓                                                              |
| PAKE protocol                | SPAKE2 (RFC 9382, IETF-standardized)                                          | SPAKE2 (`python-spake2`, symmetric mode)                       |
| Defense-in-depth layers      | **Two** independent — TLS 1.3 (QUIC) **+** SPAKE2 channel-bound to the TLS handshake (RFC 5705) | **One** — NaCl secretbox keyed from the SPAKE2 result |
| AEAD                         | TLS 1.3 ciphers (AES-128-GCM, ChaCha20-Poly1305, AES-256-GCM)                 | NaCl secretbox (XSalsa20-Poly1305)                            |
| Key derivation               | TLS 1.3 KDF + SPAKE2 + RFC 5705 exporter binding                              | HKDF-SHA256 from the SPAKE2 shared key                        |
| Post-quantum forward secrecy | ✓ X25519 + ML-KEM-768 hybrid (NIST FIPS 203)                                  | ✗ Classical elliptic-curve only                               |
| MITM defense                 | TLS catches a network MITM; SPAKE2 binding catches a TLS-handshake MITM — automatic | SPAKE2 + optional manual `--verify` string comparison         |
| Relay can decrypt            | ✗ (relay path only) — TLS terminates at the peers                             | ✗ — transit relay sees ciphertext only                        |

**The two-layer point.** A network attacker against fsend has to defeat
both TLS 1.3 *and* SPAKE2 — and SPAKE2 is bound to the TLS handshake, so a
forged certificate breaks the binding and is caught automatically before
any file data flows. magic-wormhole relies on the single SPAKE2-derived
secretbox key on the file path; its mailbox hop uses TLS (wss), but that
terminates at the server and doesn't protect the transferred bytes. It
does offer a
`--verify` mode that prints a short verification string for the two humans
to compare out-of-band — manual, and off by default.

**The post-quantum point.** fsend's TLS 1.3 key exchange uses Go's
X25519 + ML-KEM-768 hybrid, so ciphertext recorded today is not
retroactively decryptable by a future quantum computer. magic-wormhole
uses classical crypto only; recorded ciphertext is vulnerable once a
sufficient quantum computer exists.

For fsend's full threat model, see [Security](security.md).

## Share codes

The two codes carry very different amounts of entropy.

|                  | fsend                                | magic-wormhole                                |
|------------------|--------------------------------------|-----------------------------------------------|
| Format           | `abc-defg-jkm` (3-4-3 letters)       | `4-purple-sausages` (nameplate + words)       |
| Secret entropy   | **~45 bits** (log₂(23¹⁰))            | **16 bits** by default (two PGP words)        |
| Adjustable       | Fixed                                | `--code-length N` for N words (~8 bits each)  |
| One-shot         | ✓ — same code can't be reused        | ✓ — nameplate is single-use                   |
| Custom code      | ✗ (system-generated)                 | ✓ (`--code`)                                  |
| Server-side TTL  | 1 h unclaimed / 10 min after pairing | Not specified in the client docs (mailbox server is a separate project) |
| Online guessing  | Rate-limited 30/min per source IP    | No rate-limiting (acknowledged in the docs)   |
| Reaches a server | Never — only an argon2id slot        | The words never do; the nameplate (non-secret) does |

magic-wormhole's code is a short numeric **nameplate** (a non-secret
channel id the server hands out, used to find the mailbox) plus two words
drawn from the 256-entry PGP word list — **16 bits** of actual secret.
Their own [threat model](https://magic-wormhole.readthedocs.io/en/latest/attacks.html)
spells out the consequence: an attacker who controls the network or the
mailbox server has a 1-in-65536 chance of guessing the code and stepping
into the middle. The main mitigation is that a wrong guess closes the
wormhole (so each attempt is one shot per nameplate), and you can opt into
a longer code with `--code-length`.

fsend takes the opposite default: a system-generated `abc-defg-jkm` code
carries ~45 bits, never reaches the server (both peers register under an
argon2id-stretched *slot* derived from it), and the public server
rate-limits new sessions to 30/min per source IP — so online brute force
against the code space is infeasible without the operator having to think
about it. The trade-off is flexibility: magic-wormhole lets you choose
your own code phrase (`--code`) and even let the receiver allocate it
(`--allocate`); fsend always picks the code for you.

## Feature parity

Day-to-day, both cover the basics — but the edges differ.

| Capability                        | fsend         | magic-wormhole              |
|-----------------------------------|---------------|----------------------------|
| Send files                        | ✓             | ✓                          |
| Send folders                      | ✓             | ✓ (zipped, deflated)       |
| Multiple files in one transfer    | ✓             | ✗ (one file/dir per code)  |
| Send a literal string             | `--text`      | `--text`                   |
| Send arbitrary stdin stream       | ✓ (binary)    | ✗ (`--text` from stdin only)|
| Stream payload to stdout          | `--out -`     | ✗ (text prints; files saved)|
| Receiver confirmation prompt      | ✓ (`--yes`)   | ✓ (`--accept-file`)        |
| Resume after interruption         | ✓             | ✗ (classic transit restarts)|
| Skip unchanged files on re-send   | ✓             | ✗ (always re-sends everything)|
| Password on top of the code       | `--pass`      | ✗ (the code is the secret) |
| Custom code phrase                | ✗             | ✓ (`--code`)               |
| Receiver allocates the code       | ✗             | ✓ (`--allocate`)           |
| Exclude paths when sending a dir  | `--exclude`   | ✗                          |
| Custom output dir / name          | `--out`       | `--output-file`            |
| Manual verification string        | n/a (automatic binding) | ✓ (`--verify`)   |
| QR code for the code              | ✗             | ✓ (on by default)          |
| Tor support                       | ✗             | ✓ (`--tor`, `--launch-tor`)|
| SSH-key transfer helper           | ✗             | ✓ (`wormhole ssh`)         |
| Library / protocol ecosystem      | ✗ (app only)  | ✓ (importable, Dilation)   |
| Self-host the server / relay      | ✓             | ✓                          |

## Runtime and installation

| | fsend | magic-wormhole |
|---|---|---|
| Distribution | Single static binary | Python package (pip / OS repos) |
| Runtime needed | None | Python ≥ 3.10 + dependencies |
| Platforms | Linux, macOS, FreeBSD, Windows; x86 & ARM | Anywhere Python + the deps run |
| Packaging | Install script / release binary | Widely packaged in OS repos |

fsend ships as one self-contained executable with nothing to install
alongside it. magic-wormhole is a Python program: convenient where Python
and a package manager are already present (and it's packaged in many OS
repositories), but heavier to drop onto a bare server or a locked-down
machine, and subject to the usual Python-environment friction.

## When magic-wormhole is the better fit

- You need **Tor / location privacy** for the transfer.
- You want to **build on the protocol** — the Python library, Dilation,
  or the SSH-key helper.
- You like **choosing your own code phrase** or having the **receiver
  allocate** the code.
- You're on a system where a Python install is the path of least
  resistance and TCP/4001 outbound is open.
- You want the sender to display a **QR code** of the wormhole code out
  of the box.

## When fsend is the better fit

- Most of your transfers **cross the internet and throughput matters** —
  fsend hole-punches to a direct path; magic-wormhole relays whenever both
  peers are behind NAT.
- You transfer on a **LAN with no internet** — fsend pairs over mDNS with
  no server; magic-wormhole always needs to reach its mailbox.
- You care about **stronger codes by default** (~45 bits, rate-limited,
  bounded TTL) without asking the user to lengthen anything.
- You want **defense-in-depth** (TLS 1.3 *under* SPAKE2) and
  **post-quantum** forward secrecy.
- You want **resume**, a **separate `--pass`** secret, **`--exclude`**,
  **multi-file** transfers, or **binary stdin/stdout** streaming.
- You'd rather deploy **one static binary** than a Python environment.

## Summary

|                                      | fsend                                              | magic-wormhole                                     |
|--------------------------------------|----------------------------------------------------|----------------------------------------------------|
| **Cross-network data path**          | Direct when hole-punching succeeds; relay fallback | Direct only if a peer is reachable; else relayed   |
| **NAT hole-punching**                | ✓ (ICE)                                            | ✗ (Tor onion service as workaround)                |
| **Cross-network speed cap**          | Your two internet links (when direct)              | The transit relay's bandwidth (NAT-to-NAT)         |
| **Same-LAN without internet**        | ✓ (mDNS)                                           | ✗ (mailbox always required to pair)                |
| **Transport (bulk)**                 | QUIC + TLS 1.3                                      | Raw TCP + NaCl secretbox                           |
| **Firewall footprint**               | One UDP port (UDP/443)                             | wss/443 + TCP/4001                                 |
| **PAKE**                             | SPAKE2 (RFC 9382)                                  | SPAKE2 (`python-spake2`)                           |
| **Defense-in-depth layers**          | Two (TLS 1.3 + SPAKE2, channel-bound)              | One (secretbox)                                    |
| **Post-quantum forward secrecy**     | ✓ X25519 + ML-KEM-768                              | ✗                                                  |
| **Default code entropy**             | ~45 bits                                            | 16 bits (adjustable)                               |
| **Online guessing protection**       | Rate-limited + bounded TTL                          | One guess per nameplate; no rate-limit             |
| **Resume after interruption**        | ✓                                                  | ✗ (classic transit)                                |
| **Tor support**                      | ✗                                                  | ✓                                                  |
| **Distribution**                     | Single static binary                               | Python package                                     |
| **Self-hostable**                    | ✓                                                  | ✓                                                  |
