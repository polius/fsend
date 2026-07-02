# Security

## End-to-end encryption

Files are end-to-end encrypted between sender and receiver. The pairing
server never sees your filenames, sizes, hashes, or contents — not even
when it's relaying the transfer as a fallback.

Two independent layers protect every transfer:

1. **TLS 1.3 over QUIC** encrypts all file bytes. It's standard transport
   security, terminated at the two peers — never at the server.
2. **SPAKE2** proves you're talking to the right peer. It runs from the
   share code and confirms the other end of the TLS connection is the
   peer who holds that code. (SPAKE2 is a balanced PAKE — a
   password-authenticated key exchange, where two sides verify they share
   a secret without ever sending it.)

The two layers are tied together: SPAKE2 is bound to the TLS handshake
through the RFC 5705 channel-binding exporter. That binding is what
defeats a man-in-the-middle:

- A **passive eavesdropper** is stopped by TLS alone.
- An **active MITM** that terminates TLS itself — including a malicious
  pairing server or relay — ends up with a different channel binding on
  each side. The SPAKE2 confirmation catches the mismatch before any file
  data flows.

This holds even against the pairing server, because the server never
learns the share code. It sees only an argon2id-stretched *slot* derived
from the code (see [Share codes](#share-codes)), so it can't run the
SPAKE2 handshake with either side.

## Post-quantum forward secrecy

TLS 1.3's key exchange uses the **X25519 + ML-KEM-768 hybrid** (Go's
standard since 1.24). Ciphertext captured today stays secret even against
a future large-scale quantum computer.

## Per-session keys and integrity

- **Fresh TLS identity per transfer.** Each session generates a new
  Ed25519 keypair and a self-signed certificate valid for one hour.
  There's nothing to manage, rotate, or revoke — the keys die with the
  process.
- **End-to-end integrity.** Every chunk carries a BLAKE3 hash, and each
  file is checked against a BLAKE3 root over its full contents before
  it's accepted. The receiver catches corruption, not the relay.

## What the pairing server can see

It's a matchmaker, not a middleman. Its only job is to introduce two
peers who can't otherwise find each other across NAT, then step aside
(for the full mechanics, see
[How it works → Why a server?](architecture.md#why-a-server)). Even while
doing that job, it observes very little:

- Peers meet at a *slot* — a one-way argon2id stretch of the share code
  that each computes locally — so the server pairs them **without ever
  seeing the code**.
- In the common cross-network case, the file flows peer-to-peer and the
  server never sees a byte.
- When NAT topology forces the fallback relay, the server forwards
  **already-encrypted** UDP datagrams. It's a mail carrier moving sealed
  envelopes: it carries the parcel but can't open it.

### Visibility

| | Server sees |
|---|---|
| File contents | ✗ never |
| File names, sizes, hashes | ✗ never |
| Ciphertext (relay-fallback path only) | ✓ as opaque bytes — not decryptable, even by the operator |
| The share code | ✗ never — only a derived slot (see below) |
| Your IP | ✓ briefly, in memory, for pairing — never written to disk |

### What it writes to disk

Effectively nothing:

- **No access log, no per-transfer line.** The default log level emits
  only lifecycle events — startup, shutdown, and errors.
- **No IPs in the logs** at the default level, and share codes never
  reach the server at all. (`FSEND_LOG_LEVEL=debug` logs IPs and session
  slots for troubleshooting — don't run a privacy-sensitive server at
  debug level.)
- **No database, no persistence.** Pairing state lives only in RAM. It
  evicts within an hour at most — ten minutes once a transfer has paired
  — and is gone for good on restart.

If you'd still rather not trust the public server,
[self-host one](self-hosting.md): it's a single binary with nothing to
back up.

## Share codes

A code looks like `abc-defg-jkm` — three letter-groups (3-4-3) drawn from
a 23-letter alphabet (`i`, `l`, and `o` are left out for legibility).
What makes a short code safe to use as the whole secret:

- **The code never reaches the server.** Both peers register under a
  *slot* instead — a fixed-salt argon2id stretch of the code (64 MiB of
  memory, hex-encoded) — and the server just matches the two peers that
  derived the same slot. Working backward from a leaked slot to the code
  means a memory-hard brute-force of the entire ~2^45 code space: about
  2^44 argon2id evaluations at 64 MiB each, on average. That's also why
  the SPAKE2 binding holds against the server itself — not knowing the
  code, it can't run the handshake with either side.
- **One-shot.** Once a receiver claims a code, it can't be reused.
- **Time-limited.** A code expires on the server after one hour if no one
  receives, or ten minutes once a receiver has paired. Pressing Ctrl-C on
  the sender invalidates it immediately.
- **Rate-limited.** The public server is configured to cap new sessions
  at 30 per minute per source IP (keyed on the full IPv4 address, or the
  /64 prefix for IPv6), so online guessing against the ~45-bit space is
  infeasible. On a self-hosted server the cap is opt-in — it defaults to
  unlimited until you set `FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE`
  (see [self-hosting](self-hosting.md)).
- **Not the encryption key.** File data is encrypted with the TLS 1.3
  session key. The code only drives the SPAKE2 handshake that
  authenticates the peer; it never feeds the encryption key and never
  crosses the wire in the clear.
- **Chosen for you.** fsend generates the code; it isn't user-selectable.
  To add a second secret on top, send with `--password`.

## Release integrity

Each release ships a `checksums.txt` that is **signed in CI with keyless
[cosign](https://docs.sigstore.dev/)** (Sigstore Fulcio + Rekor), bound
to the release workflow's identity. The installer and `fsend --update`
then verify two separate things:

- **Authenticity** — if `cosign` is installed, the signature on
  `checksums.txt` is verified against the release workflow's identity
  before any checksum is trusted. This catches a *tampered* release, not
  just a corrupted download: a SHA-256 match alone only proves the archive
  matches `checksums.txt`, and both come from the same host.
- **Integrity** — the downloaded archive's SHA-256 is checked against the
  (now-trusted) `checksums.txt`.

cosign is optional, so the one-line install still works on hosts without
it — in that case only the checksum is verified, and the installer tells
you so. Set `FSEND_REQUIRE_SIGNATURE=1` to refuse any install that isn't
signature-verified.
