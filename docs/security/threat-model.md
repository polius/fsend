# Threat model

**Status:** Locked
**Date:** 2026-06-03
**Scope:** What fsend defends against, what it does not, and what
assumptions hold the security argument together.

---

## TL;DR (the elevator version)

fsend gives two people who can share **one short code out of band** a way
to transfer files **end-to-end encrypted** between them, with the rendezvous/
relay server as an **untrusted blind pipe**. An attacker who controls the
network, the relay, or even the relay's source code cannot read the file
content or impersonate either peer. Attackers who can read the user's
screen, compromise their machine, or guess a 45-bit code with very high
luck *can* break this — but those scenarios are outside what any tool of
this class can defend against.

## Asset being protected

- **File content** (confidentiality, integrity)
- **Peer authenticity** (Alice receives from Bob, not from an attacker
  pretending to be Bob)
- **Code phrase secrecy** (the code is meant for one transfer; it should
  not be reusable or harvested)
- **Metadata privacy** (filenames, sizes, sender hostname — limited
  privacy goals; see "non-goals" below)

## Attackers we defend against (in priority order)

### T1. Passive network attacker

**Capabilities:** Reads every packet on the path between the peers and
between each peer and the relay. Cannot inject or modify.

**Example:** Someone on the same coffee-shop WiFi, an ISP doing DPI, a
nation-state running tap on backbone links.

**fsend's defense:** TLS 1.3 over QUIC with X25519+ML-KEM-768 hybrid
key exchange. AEAD encryption on every byte. Perfect forward secrecy.
Post-quantum hybrid means "harvest now, decrypt later" attacks against
recorded traffic fail even if X25519 is broken in the future.

**Residual exposure:** Traffic timing, packet sizes, source/destination
IPs. An attacker can tell that fsend was used and approximately how much
data was sent. We do not (and cannot economically) hide this.

### T2. Active network attacker

**Capabilities:** Reads + injects + modifies + drops packets. Can attempt
classic MITM (e.g., ARP spoofing, BGP hijack, malicious WiFi).

**Example:** Hostile WiFi captive portal, compromised home router,
nation-state on-path MITM.

**fsend's defense:** TLS 1.3 mutually authenticated via PAKE channel
binding. The PAKE-derived key from the short code is mixed into the TLS
session via RFC 5705 exporter. To MITM, the attacker would need to:
1. Know the short code (~45 bits, never sent over the wire), OR
2. Break TLS 1.3 + X25519 + ML-KEM-768

Modified or injected packets fail TLS authentication and are dropped.
QUIC adds further integrity at the transport layer.

**Residual exposure:** Denial of service. An attacker can prevent a
transfer from completing by dropping packets. They cannot succeed in
impersonating a peer.

### T3. Malicious relay operator (including someone who compromises
fs.alzina.dev)

**Capabilities:** Sees and controls every byte flowing through the relay.
Can read, drop, reorder, inject.

**Example:** An attacker who pwns the `fs.alzina.dev` server. Or an ISP
operating the network on which a self-hosted relay runs.

**fsend's defense:**
- **End-to-end TLS terminates at the peers, not the relay.** The relay
  forwards opaque UDP datagrams and never holds the session keys.
- **PAKE channel binding** means the relay cannot stand up its own TLS
  session pretending to be one of the peers — it doesn't know the code,
  so the binding fails.
- **The relay does not see the code phrase.** Codes are server-generated
  but only the formatted code is returned; the PAKE input never crosses
  the wire.

**Residual exposure:** Metadata. The relay sees:
- Source IPs of both peers
- Approximate timing
- Bytes transferred per session
- The (random server-issued) code phrase used to pair the peers
- The IP address each peer was observed at (used for STUN-style reflection)

The relay does NOT see:
- File content
- Filenames
- Sender hostname
- Anything inside the QUIC session

### T4. Online code-guessing attacker

**Capabilities:** Connects to the rendezvous server repeatedly trying to
join sessions with guessed codes.

**fsend's defense:**
- 45-bit code space (~3.5 × 10^13 possibilities)
- Per-IP new-session rate limit (default 30/min)
- A guessed code is only useful while the session is alive (60s for
  unpaired, 600s after pairing)
- Even at 30 attempts/min, expected time to guess one code: ~2 million years
- The code is consumed on first successful join (server marks session as
  `paired`, second joiner gets 409 Conflict)

**Residual exposure:** Theoretical brute force from a botnet across
millions of IPs — would still take centuries on average to hit any one
session. Not a practical concern.

### T5. Malicious peer (someone who joins a session legitimately but is
who you didn't expect)

**Capabilities:** Has the code (because the user shared it, or it was
leaked). Receives the file. Or, if a sender, sends arbitrary content to
the receiver.

**Example:** User shares code in the wrong Slack channel; a coworker
joins and receives the file. User screen-shares with a confidential code
visible. User mis-dictates the code over a phone call.

**fsend's defense:**
- **Receiver-side confirmation prompt** with sender's hostname and the
  file metadata — gives the human a chance to abort if something is wrong.
- **`--pass` option** for an additional out-of-band password challenge
  beyond the code. Defends against shoulder-surfing and wrong-chat-share
  scenarios.
- **Short session TTL** (60s unpaired) — a leaked code expires quickly.

**Residual exposure:** A peer that intentionally joined a session can do
anything within the protocol (receive or send files, see the other
peer's hostname and IP via the reflected address). fsend cannot prevent
the user from sharing the code with the wrong person.

### T6. Malicious sender (someone sending malware/hostile content)

**Capabilities:** Sends arbitrary file bytes labeled as anything.

**fsend's defense:**
- **Receiver prompt shows filename and size before any bytes flow.**
  Receiver can abort.
- **Path traversal protection:** the receiver rejects `RelativePath`
  values containing `..` segments or absolute paths.
- **The receiver chooses the target directory** (`--out` or CWD); the
  sender cannot write outside it.

**Residual exposure:** The receiver accepts the file; if it's malware,
it's now on the receiver's disk. fsend is a transport, not an antivirus.
This is the same risk profile as receiving any file from any source.

### T7. Malicious receiver (someone abusing the sender's terminal)

**Capabilities:** Joins a session, sends arbitrary metadata back (e.g.,
the `Hostname` field in `HELLO_ACK` which the sender displays).

**fsend's defense:**
- **Hostname sanitization on display:** sender treats incoming hostname
  as untrusted input. Strips ANSI escape sequences, control chars,
  newlines. Truncates to 64 chars. Displays only printable ASCII +
  common UTF-8.
- **No code execution from peer data.** Filenames, hostnames, sizes are
  never `eval`'d, never passed to a shell, never used as format strings.

**Residual exposure:** Same as accepting any input from a stranger.
Bounded.

## Attackers we explicitly do NOT defend against (non-goals)

### N1. Endpoint compromise

If the sender's or receiver's machine is compromised (malware, keylogger,
disk-image attacker), fsend offers no defense. The attacker reads the
file on disk before it's sent, or after it arrives. This is true of
every file-transfer tool ever made.

### N2. Coercion / rubber-hose attacks

If someone forces a user to type the code or share the file, fsend can't
help. No tool can.

### N3. Traffic-analysis-grade metadata privacy

fsend does not hide *that* a transfer happened, *when*, between *which IP
addresses*, or *roughly how big*. Padding traffic to constant rate would
hide the bytes-transferred signal but is prohibitively expensive. Tools
like Tor (with its own latency/bandwidth costs) are better fits when this
matters.

### N4. Plausible deniability

fsend doesn't hide its own protocol fingerprint. A network observer can
tell fsend is being used (QUIC handshake signatures, known IPs of public
relays). Steganographic transports are out of scope.

### N5. Resistance to long-term cryptographic breaks beyond hybrid PQ

We use X25519+ML-KEM-768. If both are broken in 50 years, traffic recorded
today is decryptable. We accept this risk as fundamental to any current
cryptography.

## Trust boundaries (the boxes-and-arrows view)

```
   ┌─────────────────┐                  ┌─────────────────┐
   │   Alice's       │                  │   Bob's         │
   │   machine       │ ━━━ E2E TLS ━━━► │   machine       │
   │   ┌─────────┐   │  (peer-to-peer)  │   ┌─────────┐   │
   │   │ fsend   │   │                  │   │ fsend   │   │
   │   └─────────┘   │                  │   └─────────┘   │
   │                 │                  │                 │
   │   TRUSTED       │                  │   TRUSTED       │
   │   (by Alice)    │                  │   (by Bob)      │
   └────────┬────────┘                  └────────┬────────┘
            │                                    │
            │           SIGNALING                │
            │    (small JSON over HTTPS)         │
            ▼                                    ▼
   ┌──────────────────────────────────────────────────────┐
   │              fs.alzina.dev (rendezvous)              │
   │                                                      │
   │   SEMI-TRUSTED — sees IPs, code, timing, sizes       │
   │   NEVER sees file content, never inside TLS session  │
   └──────────────────────────────────────────────────────┘

            │ (when ICE fails, same server also relays:)  │
            │                                             │
            └─── opaque UDP datagrams (E2E ciphertext) ───┘
                          (BLIND PIPE)
```

## What makes the security argument hold

These are the load-bearing assumptions. If any of them fails, the security
argument cracks:

1. **The user actually shared the code only with the intended recipient.**
   Codes leak by user action, not by protocol.
2. **The PAKE library (`gospake2`) correctly implements SPAKE2.** Mitigated
   by: it's been deployed in wormhole-william for ~7 years in production.
3. **Go's stdlib TLS 1.3 + X25519+ML-KEM-768 implementation is correct.**
   Mitigated by: it's stdlib, audited, widely deployed.
4. **`pion/ice` correctly implements ICE.** Mitigated by: used in
   production by every Pion-based WebRTC deployment.
5. **The user's machine isn't compromised.** Outside our scope.
6. **Both peers run a real fsend, not a malicious fork.** Mitigated by:
   reproducible builds + signed releases (see `privacy.md`).

## Cryptographic primitives summary

| Layer | Primitive | Source |
|---|---|---|
| Code-to-key | SPAKE2 (RFC 9382, ed25519 group) | `gospake2` vendored |
| Channel binding | TLS 1.3 RFC 5705 exporter | Stdlib |
| Key exchange | X25519 + ML-KEM-768 hybrid | Stdlib (Go 1.24+) |
| Symmetric encryption | AES-128-GCM or ChaCha20-Poly1305 (TLS-negotiated) | Stdlib |
| Integrity | TLS AEAD + per-chunk BLAKE3 hash | `zeebo/blake3` |
| Password challenge (`--pass`) | HMAC-SHA256 over TLS exporter | Stdlib |
| Code generation | `crypto/rand` over 23-letter alphabet | Stdlib (server-side) |

## Security disclosure process

- Email: `security@alzina.dev`
- GPG: published on the README and at `https://alzina.dev/.well-known/security.txt`
- Response: acknowledge within 72h, fix or honest "won't fix" within 30 days
- Coordinated disclosure: 90 days from acknowledgment

We follow the same `SECURITY.md` template GitHub recommends; the file lives
at the repo root.
