# fsend compared with croc, magic-wormhole, sendme and LocalSend

All five encrypt by default and need no account. They differ in where
they work, what a server learns, and how fast they are
([Speed](#speed)).

> Checked against source code on 24 October 2026: fsend (wire protocol
> v3), croc v11.5.5, magic-wormhole 0.24.0, sendme v0.37.1, LocalSend
> v1.18.2. Out of date?
> [Open an issue](https://github.com/polius/fsend/issues).

## At a glance

| | **fsend** | **croc** | **magic-wormhole** | **sendme** | **LocalSend** |
|---|:---:|:---:|:---:|:---:|:---:|
| **Security** | | | | | |
| Peer authenticated by a shared secret | ✓ | ✓ | ✓ | Sender only¹ | ✗⁵ |
| No part of the code reaches a server | ✓ | ✗ | ✗ | n/a¹ | n/a |
| Default code strength | **~45 bits** | ~31 bits² | 16 bits | n/a¹ | n/a |
| Post-quantum key exchange | ✓ | ✗ | ✗ | ✗ | ✗ |
| Optional end-to-end password | ✓ | ✗ | ✗ | ✗ | PIN only⁶ |
| Online-guess protection | ✓ | ✓ | ✗ | n/a¹ | n/a |
| Resume only by the original receiver | ✓ | ✗ | n/a | ✗ | n/a |
| **Network** | | | | | |
| Works across the internet | ✓ | ✓ | ✓ | ✓ | ✗ |
| Direct transfer, whatever the size | ✓ | ✗³ | ✗ | ✓ | ✓ (LAN) |
| No third-party infrastructure | ✓ | ✗³ | ✓ | ✓ | ✓ |
| Works with only port 443 open | ✓ | ✗ | ✗ | ✓ | n/a |
| Same LAN, no internet | ✓ | ✓ | ✗ | Opt-in | ✓ |
| **Tooling** | | | | | |
| Received-file manifest with SHA-256 | ✓ | ✗ | ✗ | ✗ | ✗ |
| JSON output and built-in diagnostics | ✓ | ✗ | ✗ | ✗ | ✗ |
| Stdin and stdout streaming | ✓ | ✓ | ✗ | ✗ | ✗ |
| Signed releases | Checksums⁷ | ✗ | n/a⁴ | ✗ | Windows binaries |

¹ sendme uses a long ticket that names the sender's public key. It must
be copied whole, and anyone who sees it can download.
² Three words, but the relay can recover the first one, leaving ~21
bits.
³ Under 128 MiB, croc sends data through its relay; above, through
Tailscale's WireGuard with Tailscale's public DERP servers as fallback.
⁴ Distributed as a Python package on PyPI.
⁵ LocalSend pins a self-signed certificate to the fingerprint each
device announces over unauthenticated multicast, so a device on the LAN
can pose as another.
⁶ A receive PIN checked over that TLS connection, not a PAKE.
⁷ fsend signs each release's `checksums.txt` with Sigstore (cosign,
keyless) and publishes build-provenance attestations. The binaries carry
no Apple or Microsoft code signature, so a browser download meets
Gatekeeper or SmartScreen.

## What sets fsend apart

- **The server never learns the code.** It sees only argon2id(code),
  salted daily. croc's relay learns the first of its three words.
- **The code authenticates the encrypted channel itself** (CPace bound
  to TLS 1.3). A man in the middle, even the server, fails. croc's
  `--pass` only unlocks its relay; LocalSend has no shared secret.
- **Post-quantum, always.** X25519MLKEM768 on every connection; the
  others use classical key exchange.
- **Direct whatever the size, on port 443.** croc relays everything
  under 128 MiB, uses TCP 9009–9017, and needs Tailscale for larger
  files. LocalSend works only on the local network.
- **Verifiable.** Per-file BLAKE3 checks, resume only from the original
  device, `--manifest` with SHA-256, `--json` and `fsend doctor`.

## Speed

Both ends on one machine (Apple M2 Pro), so the network is out of the
picture: what's left is each tool's own overhead. Time from starting the
sender to the receiver finishing, with every byte written and checked.
Median of 3 or more runs.

| | **fsend** | **croc** | **magic-wormhole** | **sendme** | **LocalSend** |
|---|---:|---:|---:|---:|---:|
| **One 1 GiB file** (random data) | | | | | |
| Local network | 1.2 s<br>~940 MB/s | 1.9 s<br>~580 MB/s | 21 s<br>~52 MB/s | 25 s<br>~43 MB/s¹ | **0.7 s**<br>~1,500 MB/s⁵ |
| Direct between networks | **21 s**<br>~52 MB/s | 34 s<br>~32 MB/s² | 21 s<br>~52 MB/s³ | 25 s<br>~43 MB/s⁴ | local network only |
| **A folder of 5,000 files of 20 KiB** (102 MB) | | | | | |
| Local network | **1.7 s**<br>~61 MB/s | 9.1 s<br>~11 MB/s | 5.8 s<br>~18 MB/s | 35 s<br>~3 MB/s | 2.3 s<br>~45 MB/s⁵ |
| Direct between networks | **2.5 s**<br>~41 MB/s | 9.8 s<br>~10 MB/s, relayed² | 5.8 s<br>~18 MB/s³ | 35 s<br>~3 MB/s⁴ | local network only |


- **Local network:** fsend, croc and magic-wormhole use TCP, LocalSend
  HTTPS, sendme QUIC.
- **Direct between networks**, forced on one machine: fsend with
  `--mode direct` (QUIC), croc with Tailscale's WireGuard.
- Random data, so compression doesn't help anyone.
- Between two real machines, the network and disks add to these times.

¹ Median of 7 runs. 3 were much slower (22, 9, 9 MB/s), probably through
iroh's relay.
² croc sends files of 128 MiB or more over Tailscale's WireGuard
(`--transport derp`; every run reported `path=direct`), and smaller
transfers through its relay (measured with a relay on the same machine).
³ Same connection as on the local network. magic-wormhole can't punch
through NAT: between networks it's direct only if one side accepts
incoming connections.
⁴ Same QUIC path as on the local network.
⁵ `localsend-cli`, driven through a pty. Its time starts when the device
is picked, so it leaves out the startup and discovery the others
include. The folder went as 5,000 files.

Measured on 16 November 2026: fsend 1.17.0 (pre-release), croc v11.6.5,
magic-wormhole 0.24.0, sendme v0.37.1, LocalSend CLI v1.18.2. croc
v11.5.5 failed 2 of 3 runs, so the table uses v11.6.5.

## Trade-offs

- croc has a browser receiver, QR codes, custom code phrases,
  store-and-forward and SOCKS5.
- magic-wormhole supports Tor and ships a Python library.
- sendme serves one send to many receivers, and can pair with no server
  when one side is directly reachable.
- LocalSend has graphical apps for Android, iOS, macOS, Windows and
  Linux, and lets a browser on the LAN receive through a link.

fsend is a command-line tool: one code, one receiver, by design.

## Sources

- **fsend:** this repository; see [architecture](architecture.md) and
  [security](security.md).
- **croc** [v11.5.5](https://github.com/schollz/croc/tree/v11.5.5):
  `src/codephrase/codephrase.go` (word list, room hash),
  `src/croc/tailcat_negotiation.go` (128 MiB threshold),
  `src/tcp/defaults.go` (join caps), `disco.json` (relay ports).
- **magic-wormhole** [0.24.0](https://github.com/magic-wormhole/magic-wormhole/tree/0.24.0):
  `cli/cli.py`, `cli/public_relay.py`; its
  [threat model](https://magic-wormhole.readthedocs.io/en/latest/attacks.html)
  notes the 16-bit default has no rate limiting.
- **sendme** [v0.37.1](https://github.com/n0-computer/sendme/tree/v0.37.1):
  `src/main.rs`, `Cargo.toml`, iroh's default relays.
- **LocalSend** [v1.18.2](https://github.com/localsend/localsend/tree/v1.18.2):
  `app/lib/provider/network/webrtc/signaling_provider.dart`
  (`webRTCEnabled = false`: internet transfer is not enabled),
  `packages/core/src/http/client/` (rustls with the `ring` provider,
  certificate pinned to the announced fingerprint),
  `packages/core/src/http/server/mod.rs` (receive PIN), and
  `CODE_SIGNING.md` (Windows signing via SignPath).
