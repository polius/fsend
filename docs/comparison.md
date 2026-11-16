# fsend compared with croc, magic-wormhole, sendme and LocalSend

All five tools encrypt transfers by default and need no account. They differ in
where they work, what a server learns, how the bytes travel, how
strongly each side knows who is on the other end, and how fast they
are ([Speed](#speed)).

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
(`fsend server`). At its default log level the server logs no client
IPs, and it exposes metrics
only on a private listener. Releases are signed with cosign and carry
GitHub attestations; croc publishes checksums only.

## Speed

Both ends on one machine (Apple M2 Pro, macOS), so the network is out
of the picture and what remains is each tool's own overhead: crypto,
protocol and disk. Time from starting the sender to the receiver
exiting with every byte written to disk and checked against the
original; median of three runs or more.

| | **fsend** | **croc** | **magic-wormhole** | **sendme** |
|---|---:|---:|---:|---:|
| One 1 GiB file (random data) | **1.2 s**<br>~940 MB/s | 1.9 s<br>~580 MB/s | 21 s<br>~52 MB/s | 25 s<br>~43 MB/s¹ |
| A folder of 5,000 files of 20 KiB (102 MB) | **1.7 s**<br>~61 MB/s | 9.1 s<br>~11 MB/s | 5.8 s<br>~18 MB/s | 35 s<br>~3 MB/s |

- fsend, croc and magic-wormhole move the data over TCP (croc through
  a relay on the same machine), sendme over QUIC on UDP.
- Random data, so croc's compression doesn't help it. With data that
  compresses, fsend's zstd helps too.
- Between two machines the link and the disks are usually the limit.
  These numbers show what each tool needs on top of that, and how far
  it is from saturating a fast local network.
- LocalSend isn't measured: it has no command-line client to script.

¹ Median of seven runs. Three were far slower (22, 9 and 9 MB/s). A run
traced with debug logs used the direct path at about 49 MB/s; the slow
ones likely fell back to iroh's public relay.

Measured on 16 November 2026 with fsend 1.17.0 (pre-release build of
`main`), croc v11.6.5, magic-wormhole 0.24.0 and sendme v0.37.1. fsend
and croc paired through a server running on the same machine,
magic-wormhole through its public mailbox, sendme by ticket. croc
v11.5.5 failed two of three 1 GiB runs ("could not secure channel"), so
the table uses the current release. fsend's limits on each route,
including between networks, are in the
[FAQ](faq.md#how-fast-is-it).

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
