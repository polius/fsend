# Security

## End-to-end encryption

Files are end-to-end encrypted between sender and receiver. The pairing
server never sees your filenames, sizes, hashes, or contents — not even
when it's relaying the transfer as a fallback.

Every transfer runs over **TLS 1.3 (inside QUIC), authenticated by a PAKE
that is bound to that TLS session**. These are not two independent
layers; each covers what the other can't:

1. **TLS 1.3 over QUIC** encrypts and integrity-protects every byte.
   It's terminated at the two peers, never at the server. On its own it
   authenticates nothing: the sender presents a fresh, self-signed
   certificate that the receiver does not verify, and the receiver
   presents none.
2. **CPace** (draft-irtf-cfrg-cpace-21, suite CPACE-X25519-SHA512)
   supplies the authentication. It's a balanced PAKE — a
   password-authenticated key exchange in which both sides prove they
   hold the same share code without sending it. CPace is the balanced
   PAKE the IRTF CFRG selected; fsend's implementation is checked
   against the draft's test vectors.

The two are tied together by the TLS exporter (RFC 5705 keying-material
export). Both peers export a value unique to their TLS session and use it
as the CPace session id, then exchange key-confirmation tags that cover
the exporter, the full PAKE transcript and the direction (the tag the
sender sends differs from the one it expects, so a tag can't be reflected
back). That binding is what defeats a man-in-the-middle:

- A **passive eavesdropper** sees only TLS ciphertext.
- An **active MITM** that terminates TLS itself — including a malicious
  pairing server or relay — has a different TLS session with each side,
  so the exporters differ and the confirmation tags can't match. The
  handshake fails (`E022`) before any file data flows.

This holds against the pairing server as long as it doesn't know the
share code, and it never receives the code: it sees only an
argon2id-stretched *slot* derived from it (see [Share codes](#share-codes)).
Without the code it can make one online guess per handshake, like anyone
else.

Because the PAKE is the only authentication, the share code is the whole
secret. Anyone who has it — and gets to the sender before the intended
receiver — can receive the transfer.

### `--password`

`--password` runs a **second, mutual CPace exchange** inside the
already-authenticated session: the password is the secret, the session's
binding (derived from the first PAKE) is the channel identifier, and a
fresh random nonce is the session id. Both sides send confirmation tags,
so the sender also proves it knows the password. Nothing on the wire —
not even to the peer — gives an offline dictionary attack something to
work on; every attempt is one online guess.

The sender allows three attempts per pairing and three pairing rounds per
send, so a leaked code buys at most nine password guesses.

## Post-quantum key exchange

The TLS 1.3 key exchange between peers is the **PQ-hybrid X25519MLKEM768**
(X25519 combined with ML-KEM-768, NIST FIPS 203), and it is **required**:
fsend offers no other group, so there is no classical fallback for an
attacker to downgrade to. A peer that can't negotiate it fails the
handshake. This protects recorded traffic against a future quantum
computer ("harvest now, decrypt later").

It covers the peer-to-peer connection only. The HTTPS connection to the
pairing server uses Go's default TLS settings and whatever the server
supports, and the PAKE itself (CPace on X25519) is classical: a quantum
attacker who recorded a handshake could attack the code offline later.
That would let them learn the code, not decrypt the transfer, whose keys
come from the hybrid TLS exchange.

## Per-session keys and integrity

- **Fresh TLS identity per session.** The sender's QUIC listener
  generates a new Ed25519 key pair and a self-signed certificate valid
  for one hour; it lives only in memory. It is not verified (the PAKE
  authenticates the peer), so there's nothing to manage, rotate, or
  revoke.
- **End-to-end integrity.** QUIC's AEAD protects every packet in
  transit. On top of that, each file carries a BLAKE3 root computed over
  1 MiB blocks and the file size; the receiver recomputes it from what
  it wrote and rejects the file on a mismatch (`E011`). When a transfer
  resumes, the receiver's existing prefix is checked against the
  sender's root over the same prefix; if it doesn't match, that file is
  sent again from the start.

## What the pairing server can see

It's a matchmaker, not a middleman. Its only job is to introduce two
peers who can't otherwise find each other across NAT, then step aside
(for the full mechanics, see
[How it works → Why a server?](architecture.md#why-a-server)). Same-LAN
transfers don't involve it at all.

What a server operator can observe:

- **The slot** for each transfer — a 32-character value derived from the
  code with argon2id, salted with the UTC day — plus a random session ID
  and per-role bearer tokens. After the first successful handshake the
  peers re-pair under a slot derived from their session secret instead,
  which can't be linked back to the code.
- **IP addresses and timing:** your public IP and port, and the ICE
  candidates each peer publishes (the LAN addresses and ports your peer
  may connect to directly). VPN, container, VM and link-local interfaces
  are never published.
- **Your fsend version** (sent with each pairing request) and the wire
  protocol version.
- **On the relay path:** the volume and timing of the encrypted traffic.

What it can't observe: the share code, file names, sizes, hashes,
contents, the device names the peers show each other, or a `--password`.
All of those travel inside the peer-to-peer TLS session.

### Visibility

| | Server sees |
|---|---|
| File contents | ✗ never |
| File names, sizes, hashes | ✗ never |
| Device names | ✗ never (exchanged inside the encrypted session) |
| Ciphertext (relay-fallback path only) | ✓ as opaque bytes — not decryptable, even by the operator |
| The share code | ✗ never — only a derived slot (see below) |
| Slot, session ID, role tokens | ✓ in memory, for the life of the session |
| Your IP | ✓ in memory, for pairing — not written to disk at the default log level |
| Your local addresses | ✓ the LAN addresses your peer may connect to directly (ICE host candidates), relayed in memory during pairing. VPN, container, VM and link-local interfaces are never published |
| Your fsend version | ✓ sent with every pairing request |

### What it writes to disk

Very little:

- **No per-transfer line.** The default log level emits lifecycle events
  (startup, shutdown, errors) and one line per *failed* request, naming
  only the method, route pattern (e.g. `/v1/session/{slot}/join`),
  status, size and duration.
- **No IPs or slots in the logs** at the default level, and share codes
  never reach the server at all. (`FSEND_LOG_LEVEL=debug` adds the client
  IP and the request path, which carries the slot, for troubleshooting —
  don't run a privacy-sensitive server at debug level.)
- **No database.** Pairing state lives in RAM. An unclaimed session
  expires after an hour (or five minutes after its sender stops polling),
  a paired one after ten minutes, and a used code's tombstone after ten
  minutes. The one exception is a restart with `FSEND_STATE_FILE` set
  (the bundled compose file sets it): pending, unpaired sessions are
  saved at shutdown so their codes survive a redeploy. That file holds
  slots, session IDs, sender tokens and ICE credentials — no IPs, no
  codes — and is deleted as soon as the next process reads it.
- **Metrics, if enabled,** are aggregate counters on a separate listener
  (`FSEND_METRICS_ADDR`); nothing per IP, session or code.

If you'd still rather not trust the public server,
[self-host one](self-hosting.md): it's a single binary with nothing to
back up.

## Share codes

A code looks like `abc-defg-jkm` — three letter-groups (3-4-3) drawn from
a 23-letter alphabet (`i`, `l`, and `o` are left out for legibility),
about 45 bits. What makes a short code safe to use as the whole secret:

- **The server sees a slot, not the code.** Both peers register under a
  *slot*: argon2id of the code (64 MiB of memory, 2 passes), salted with
  a domain label and the current UTC day, then cut to 16 bytes with HKDF
  and hex-encoded. The server only matches the two peers that derived
  the same slot. Recovering a code from a captured slot means
  brute-forcing the ~2^45 code space at 64 MiB per guess (about 2^44
  argon2id evaluations on average), and because the day is in the salt,
  a precomputed table stops working at midnight UTC. Near midnight a
  receiver also tries the neighbouring day. The mDNS name a sender
  announces on the LAN is derived from the same stretch with a different
  label, so a LAN observer faces the same cost.
- **One receiver.** The first receiver to complete the handshake owns
  the transfer. From then on both peers re-pair only on a *resume key*
  derived from their session secret — a fresh rendezvous for discovery
  and a 256-bit PAKE secret — never on the code again. The code's server
  slot is tombstoned for ten minutes, so another receiver using the code
  in that window gets `E003` ("already claimed"); after that it gets
  `E002`. A sender stops with `E046` after 10 failed handshakes on its
  code.
- **Resume only from the original device.** The receiver saves its
  resume key in the fsend config directory (mode `0600`, kept up to 24
  hours, deleted when the transfer completes), so rerunning the same
  `fsend <code>` on that device reconnects to a sender that is still
  running. Someone else holding the code can't take over a dropped
  transfer.
- **Time-limited.** Each pairing round waits up to one hour for a
  receiver (the server drops an unclaimed session after an hour, or five
  minutes after its sender stops polling). Pressing Ctrl-C on the sender
  ends it immediately. Once the sender exits, the code is gone; resuming
  then takes a new code.
- **Rate-limited.** The public server is configured to cap new sessions
  and joins at 30 per minute per source IP (keyed on the direct connection's IPv4 address, or the
  /64 prefix for IPv6, with 4× that budget per /48 so rotating /64s inside
  one site doesn't help — proxy headers are never trusted, since a client
  could otherwise rotate its "IP" past the cap). Each guess also costs
  an argon2id derivation to find the slot, works only against a live
  session, and a sender gives up after 10 failed handshakes. On a self-hosted server the per-IP caps are
  opt-in — they default to unlimited until you set
  `FSEND_SERVER_MAX_SESSIONS_PER_IP_PER_MINUTE` and
  `FSEND_SERVER_MAX_SESSIONS_PER_IP` (see [self-hosting](self-hosting.md)).
- **Not the encryption key.** File data is encrypted with the TLS 1.3
  session keys. The code only drives the CPace handshake that
  authenticates the peer; it never feeds the encryption key and never
  crosses the wire, in the clear or otherwise.
- **Chosen for you.** fsend generates the code; it isn't user-selectable.
  To add a second secret on top, send with `--password`.

## Relay threat model

The relay forwards datagrams between two peers that the pairing server
has already introduced. Its protections are about not being abused, not
about confidentiality (the traffic is already end-to-end encrypted):

- **Allocation needs a session.** A relay allocation is requested over
  signaling with a session's role token, and the relay token it returns
  is random (128 bits).
- **Return-routability cookie.** Over UDP, a peer's address is recorded
  only after a challenge: the relay answers a `HELLO` with a cookie
  (HMAC over a server secret, a 30-second epoch, the relay token and the
  source address) sent to that address, and only a `REGISTER` that echoes
  it registers the peer. A spoofed source never sees the cookie, so it
  can't aim the relay at a third party. Control replies are fixed-size,
  so they don't amplify, and challenge replies are rate-limited per
  source prefix.
- **Two peers per allocation.** Each side registers under a stable
  private peer ID derived from its role token; a third ID is refused.
  A peer whose address changes (a network switch) re-registers with a
  fresh challenge.
- **TCP fallback.** A peer that attaches over the WebSocket fallback
  (`/v1/relay/ws`) authenticates with its session role token (and the
  server password, if one is set); a TCP connection is already
  return-routable, so it registers without a cookie.

What a malicious relay can do: drop, delay or reorder traffic (the
transfer stalls or fails), and observe volume and timing. It can't read
or alter the transfer, and it can't impersonate a peer without the code.

## Resume-key threat model

- The resume key is derived from the session binding, a secret only the
  two peers that completed the first CPace run hold. It is not derivable
  from the code, so a shoulder-surfed code can't take over a dropped
  transfer, and the new rendezvous slot can't be linked to the old one.
- The receiver's copy is stored unencrypted (mode `0600`) in its config
  directory, under a file name hashed from the code. Anyone who can read
  your config directory while that file exists could reconnect to a
  sender that is still running, which is roughly what they could do by
  running fsend as you anyway.
- The sender keeps its copy only in memory.

## Release integrity

Each release ships a `checksums.txt` with the SHA-256 of every archive
and of the two install scripts, plus:

- **A keyless Sigstore signature** over `checksums.txt`
  (`checksums.txt.sigstore.json`), made by cosign from the release
  workflow, so the signing identity is that workflow at a release tag.
- **GitHub artifact attestations** (SLSA build provenance) for every
  archive, binary, `checksums.txt` and installer.
- **An SBOM per archive** (`*.sbom.json`, generated by syft).
- **Reproducible builds.** Builds use `-trimpath`, no build ID, and the
  commit's timestamp for the embedded date and file times; CI rebuilds a
  snapshot and checks it is byte-identical.

To verify a download yourself:

```sh
# 1. checksums.txt was signed by fsend's release workflow
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/polius/fsend/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# 2. the archive matches its line in checksums.txt
sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c checksums.txt

# or, instead of both: check the build provenance
gh attestation verify fsend_<version>_linux_amd64.tar.gz --repo polius/fsend
```

What the tools check for you:

- The **install scripts** verify the archive against `checksums.txt`
  before installing. When `cosign` is installed they first verify
  `checksums.txt`'s signature and abort if it is bad; without cosign
  (or for an old release with no signature) they fall back to the
  checksum alone, with a warning in the latter case.
- **`fsend --update`** downloads the new release's own install script,
  checks it against that release's `checksums.txt` (checking the
  signature first when cosign is installed), and runs it pinned to that
  version; the script then verifies the archive as above.
- **Homebrew** installs carry the same attestation; the cask's caveats
  show `gh attestation verify "$(brew --prefix)/bin/fsend" --repo polius/fsend`.

A checksum fetched from the same place as the archive only proves
integrity. The signature and the attestations are what tie a release to
fsend's workflow, so if you need authenticity, install cosign or use
`gh attestation verify`.
