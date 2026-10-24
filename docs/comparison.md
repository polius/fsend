# fsend compared with croc, magic-wormhole, sendme and LocalSend

All five tools encrypt transfers by default and need no account. They differ in
where they work, what a server learns, how the bytes travel, and how
strongly each side knows who is on the other end.

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
| Signed release binaries | ✓ | ✗ | n/a⁴ | ✗ | Windows only |

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

## What sets fsend apart

**The server never learns the code.** Peers register under
argon2id(code), salted with the UTC day, so reversing what the server
holds means a memory-hard search of a ~2^45 code space, restarted
daily. croc's relay room is `SHA-256(first word + "croc")`, and its
1,296-word list can be enumerated, so the relay learns a third of the
code.

**Authentication covers the channel itself.** fsend runs CPace with the
TLS 1.3 exporter as its session ID, so the PAKE authenticates the
exact encrypted session the file travels over. A man-in-the-middle,
including a malicious server or relay, fails before any data flows.
`--password` adds a second, mutual CPace. croc's `--pass` only grants
access to the relay, and LocalSend has no shared secret at all: it
trusts the certificate fingerprint a device announces.

**Post-quantum by default, and only.** Every peer connection negotiates
X25519MLKEM768, and classical-only peers are refused, so recorded
traffic stays confidential against a future quantum computer. The
others use classical key exchange.

**Direct for every transfer, on standard ports.** The server introduces
the peers over ICE and steps aside. It relays encrypted packets only
when NAT defeats hole-punching, and over a WebSocket on TCP/443 when UDP
is blocked. croc relays every transfer under 128 MiB, uses TCP
9009–9017, and depends on Tailscale for larger files even when you
self-host. LocalSend works only on the local network.

**Transfers you can verify.** Each file is checked against a BLAKE3
root. Resume re-verifies the partial file and works only from the
original receiver device; anyone else using the code gets `E003`.
`--manifest` records each file's SHA-256, `--preview` shows what would
be sent, `--json` serves scripts, and `fsend doctor` diagnoses
connectivity.

**One binary, private by default.** The client is also the server
(`fsend server`). The server logs no client IPs and exposes metrics
only on a private listener. Releases are signed with cosign and carry
GitHub attestations; croc publishes checksums only.

## Trade-offs

- croc has a browser receiver, QR codes, custom code phrases,
  store-and-forward and SOCKS5.
- magic-wormhole supports Tor and ships a Python library.
- sendme serves one send to many receivers, and can pair with no server
  when one side is directly reachable.
- LocalSend has graphical apps for Android, iOS, macOS, Windows and
  Linux, and lets a browser on the LAN receive through a link.

fsend is a command-line tool with one code and one receiver, by design,
and pairs across networks through its server.

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
