# Security

## End-to-end encryption

Files are end-to-end encrypted between sender and receiver. The pairing
server cannot read filenames, file sizes, hashes, or contents — even
when it's in the data path as a relay fallback.

The encryption stack uses **two layers** that an attacker has to defeat
independently:

1. **TLS 1.3 over QUIC.** Standard transport security, terminated at the
   two peers (not at the server).
2. **SPAKE2 (RFC 9382)** authenticated from the share code, bound to the
   TLS handshake via the RFC 5705 channel-binding exporter.

A network MITM is caught by TLS. A TLS-handshake MITM is caught by
SPAKE2 binding. Compromising a transfer requires breaking **both**, not
either.

## Post-quantum forward secrecy

TLS 1.3's key exchange uses the **X25519 + ML-KEM-768 hybrid** (Go's
standard since 1.24). Ciphertext captured today is not retroactively
decryptable by a future large-scale quantum computer.

## Is the pairing server something to worry about?

No. It's a matchmaker, not a middleman — and even when it has to step in
as a fallback, it can't read your files.

### Why the server has to exist

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
| Ciphertext (on the relay-fallback path)           | ✓ as opaque bytes — not decryptable, not even by the server's operator |
| The 10-letter pairing code and your IP            | ✓ briefly, in memory only, for pairing — never written to disk |

End-to-end encryption means even the operator of the server can't
decrypt traffic that goes through it. The encryption keys never leave
the two peers.

### What it writes to disk

Effectively nothing:

- **No access log. No per-transfer log line.** The default log level
  only emits lifecycle events — startup, shutdown, and errors.
- **No IP addresses or pairing codes in logs**, at any level.
- **No database, no persistence layer.** Pairing state lives in RAM,
  evicts within an hour at most (ten minutes once a transfer has
  paired), and is gone forever on restart.

If you'd still rather not trust our public server,
[self-host one](self-hosting.md) — it's a single binary with nothing to
back up.

## Share codes

Codes look like `abc-defg-jkm` — three letter-groups (3-4-3) from the
23-letter alphabet `abcdefghjkmnpqrstuvwxyz` (`i`, `l`, `o` are
excluded for legibility). Codes are:

- **One-shot** — once claimed by a receiver, the same code can't be reused.
- **Short-lived** — codes expire on the server after 60 seconds if unclaimed.
- **Not the encryption key** — the code authenticates the SPAKE2 handshake;
  the actual session key is derived from that handshake plus the TLS 1.3
  channel binding, so the code itself never traverses the wire in the clear.
- **System-generated** — fsend picks the code for each transfer. For
  persistent shared secrets across many transfers (scripts, kiosks),
  use `--pass` instead.

## Compared to croc

|                                  | fsend                                              | croc                                              |
|----------------------------------|----------------------------------------------------|---------------------------------------------------|
| End-to-end encrypted             | ✓                                                  | ✓                                                 |
| PAKE protocol                    | SPAKE2 (RFC 9382, IETF-standardized)               | `schollz/pake` (non-standardized, Boneh-Shoup textbook construction) |
| Crypto layers over the wire      | TLS 1.3 over QUIC **plus** SPAKE2, channel-bound via the RFC 5705 exporter | Single AEAD (AES-GCM or XChaCha20-Poly1305) keyed directly from PAKE |
| Post-quantum forward secrecy     | ✓ (X25519 + ML-KEM-768 hybrid)                     | ✗ (classical ECC only)                            |
| MITM defense                     | Two layers — TLS catches a network MITM, SPAKE2 binding catches a TLS-handshake MITM | Single layer — PAKE alone                         |
| Relay does not see the file      | ✓ — relay only forwards opaque UDP datagrams; QUIC/TLS terminate at the peers | ✓ — relay only forwards ciphertext after PAKE     |
| How often the relay is in path   | Fallback only (used when ICE hole-punching fails)  | Every cross-network transfer                      |
