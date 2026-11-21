# Security

## In short

- Files are encrypted end to end, with TLS 1.3 and a post-quantum key
  exchange.
- The share code authenticates both sides through a PAKE bound to the
  TLS session. A man in the middle, even the server, can't get in.
- The server never sees the code, file names, sizes or contents.
- The code is the whole secret: whoever uses it first receives the
  transfer. Add `--password` for a second secret.

## Encryption and authentication

Every route runs **TLS 1.3** between the two devices, never ending at
the server. On the local network that's TLS over TCP (QUIC as a
fallback). Across the internet it's QUIC.

TLS alone authenticates nothing: the sender's certificate is
self-signed and not checked. Authentication comes from **CPace**
(draft-irtf-cfrg-cpace-21, CPACE-X25519-SHA512), a balanced PAKE. Both
sides prove they know the code without sending it.

**Binding.** CPace uses the TLS exporter (RFC 5705) as its session id.
Both sides then exchange confirmation tags over the exporter, the PAKE
transcript and the direction. A man in the middle has a different TLS
session with each side, so the tags can't match, and the handshake fails
(`E022`) before any data moves.

**Order.** The receiver sends its tag first, and the sender answers only
a correct one. So every guess against a sender is a wrong tag that the
sender sees and counts. A peer that leaves without a tag learns nothing.

**`--password`** runs a second, mutual CPace inside the session, with
the password as the secret. Every attempt is one online guess; nothing
allows an offline attack. The sender allows 3 attempts per pairing and 3
pairings per send: at most 9 guesses.

## Post-quantum key exchange

The TLS key exchange between the devices is **X25519MLKEM768** (X25519
plus ML-KEM-768, FIPS 203). It's the only option offered, so there's no
classical fallback to downgrade to. This protects recorded traffic
against a future quantum computer.

Not covered: the HTTPS connection to the server (Go's defaults), and
CPace itself, which is classical. A quantum attacker with a recorded
handshake could recover the code, but not decrypt the transfer.

## Keys and integrity

- **Fresh identity per session.** The sender makes a new Ed25519 key and
  a one-hour self-signed certificate, kept in memory only.
- **TCP route.** The file data travels on a second TLS connection. It
  counts only after both sides send a tag tying it to the session.
- **Integrity.** TLS protects every byte in transit. Each file also
  carries a BLAKE3 root (1 MiB blocks, plus the size). The receiver
  recomputes it and rejects a mismatch (`E011`). A resumed file's
  existing part is checked the same way.

## What the pairing server can see

The server introduces two devices, then steps aside
([why a server](architecture.md#why-a-server)). Every send registers
with it, even on one network, unless you use `--local`.

| | Server sees |
|---|---|
| File contents, names, sizes, hashes | Never |
| Device names | Never |
| The share code | Never, only a value derived from it (the slot) |
| `--password` | Never |
| Slot, session ID, role tokens | Yes, in memory, while the session lasts |
| Your public IP | Yes, in memory. Not logged by default. |
| Your local addresses | Yes, the ones offered for a direct connection. Link-local, VPN, container and VM interfaces are never sent. |
| Your fsend version | Yes |
| Relayed data | Only as encrypted bytes, with their size and timing |

After the first handshake, the two devices re-pair under a slot derived
from their session secret. It can't be linked to the code.

**What it writes to disk:** almost nothing.

- No database. Sessions live in memory and expire within an hour.
- No line per transfer. At the default log level: startup, shutdown,
  errors, and one line per failed request (route and status, no IP or
  slot). `FSEND_LOG_LEVEL=debug` adds IPs and slots.
- Metrics, if enabled, are totals only, on a separate port.

Don't want to trust the public server? [Run your own](self-hosting.md).

## Share codes

A code like `abc-defg-jkm` has about **45 bits**: three groups of letters
from a 23-letter alphabet (no `i`, `l`, `o`).

- **The server sees a slot, not the code.** The slot is argon2id of the
  code (8 MiB, 5 passes, a little above one of OWASP's recommended
  settings), salted with the UTC day, cut to 16 bytes with HKDF.
  Reversing it means about 2^44 argon2id runs, far more than fit in the
  hour a code lives, and the daily salt makes precomputed tables useless
  after midnight. The mDNS name on the local network costs the same to
  reverse. Up to 1.17 the stretch used 64 MiB and 2 passes: each guess
  cost about three times as much, and fsend's memory peak was more than
  twice as high.
- **One receiver.** After the handshake, both sides switch to a resume
  key from their session secret. The code is marked used for 10 minutes
  (`E003`, then `E002`). A receiver that joins but never finishes the
  handshake doesn't use up the code.
- **Resume from the same device only.** The receiver keeps the resume
  key in its config folder (mode `0600`, up to 24 hours). Someone else
  with the code can't take over a dropped transfer.
- **Time-limited.** Each wait lasts up to an hour. The code dies when the
  sender exits.
- **Rate-limited.** The public server allows 30 new sessions and joins
  per minute per IPv4 address or IPv6 /64 (4× that per /48). It trusts
  `X-Forwarded-For` only from proxies in `FSEND_SERVER_TRUSTED_PROXIES`.
  On your own server these limits are off until you set them.
- **Failed handshakes slow the sender.** After 10 on the local network,
  it stops listening there and waits through the server.
- **Not the encryption key.** The code only drives the PAKE. It never
  crosses the network and never feeds the encryption keys.
- **Chosen by fsend.** You can't pick your own code.

## The relay

The relay forwards encrypted data. Its protections stop abuse; secrecy
comes from the encryption.

- **Needs a session.** An allocation requires a session's role token,
  and returns a random 128-bit relay token.
- **Return-routability.** Over UDP, a peer registers only by echoing a
  cookie (an HMAC over a server secret, a 30-second epoch, the token and
  its address). A spoofed address never sees the cookie, so the relay
  can't be aimed at someone else. Replies don't amplify and are rate
  limited.
- **Two peers per allocation.** A third is refused.
- **TCP fallback** (`/v1/relay/ws`) authenticates with the role token.

A malicious relay can drop, delay or watch the volume of traffic. It
can't read it, change it, or pretend to be a peer.

## The resume key

- It's derived from a secret only the two devices hold, not from the
  code.
- The receiver stores it unencrypted (mode `0600`) in its config folder.
  Anyone who can read that folder could already run fsend as you.
- The sender keeps it in memory only.

## Release integrity

Each release includes:

- `checksums.txt`: SHA-256 of every archive, package and install script.
- `checksums.txt.sigstore.json`: a keyless Sigstore signature, made by
  the release workflow.
- GitHub build attestations for every archive, binary, `checksums.txt`
  and installer. Linux packages are covered through `checksums.txt`.
- An SBOM per archive.
- Reproducible builds: CI rebuilds and checks the bytes match.

To verify a download:

```sh
# 1. checksums.txt was signed by fsend's release workflow
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/polius/fsend/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# 2. the file matches checksums.txt
sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c checksums.txt

# or, instead of both: check the build provenance
gh attestation verify fsend_<version>_linux_amd64.tar.gz --repo polius/fsend
```

What's checked for you:

- **Install scripts** check the archive against `checksums.txt`. With
  `cosign` installed, they also check the signature. Without it, they
  only prove the download is intact, not who built it.
- **`fsend --update`** checks the new install script against the
  release's `checksums.txt` before running it.
- **Homebrew:** `gh attestation verify "$(brew --prefix)/bin/fsend" --repo polius/fsend`.

**The install address.** `getfsend.alzina.dev` only redirects to the
latest release's `install.sh` (and `/windows` to `install.ps1`). Whoever
controls that domain controls what the one-line install runs. To trust
GitHub alone, use
`https://github.com/polius/fsend/releases/latest/download/install.sh`
directly.
