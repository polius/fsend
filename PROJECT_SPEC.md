# fsend — P2P File Transfer Tool — Project Spec

A secure, reliable, truly peer-to-peer file transfer CLI. A successor to
[croc](https://github.com/schollz/croc): same short-code UX and easy install,
but with a real transport, real NAT traversal so the data path is *direct*
(not relayed), and cryptographic integrity.

**Project name:** `fsend`
**Local repo path:** `/Users/palzina/git/fsend/`
**Default server host:** `fs.alzina.dev` — one host fills three roles: signaling (HTTPS/443), STUN-style address reflection, and TURN-style relay fallback (UDP/443).
**License:** MIT (matches croc; permits reuse with attribution)
**Feature scope for v1:** full parity with croc (single files, multi-file,
directories, stdin, optional compression, resume, code-based pairing).
Nothing is deferred to v2.

## Core goal

Two people share a short code phrase out of band; one sends, one receives.
The file flows **directly peer-to-peer whenever possible**, with a relay used
only as a fallback for hard NATs. Encryption is modern and vetted. Install is a
single `curl | sh`. The server is a tiny self-hostable Docker image.

## What croc does today (and what we change)

| Layer | croc | This project | Why |
|---|---|---|---|
| Language | Go | **Go** | Static binaries, effortless cross-compile, native `curl \| sh` story. Keep. |
| Transport | Multiple parallel TCP ports (9009–9013), hand-rolled framing | **QUIC (`quic-go`)** | One socket: stream multiplexing, TLS 1.3, congestion control, connection migration. Replaces the multi-port hack and custom framing. Easier to hole-punch. |
| Encryption | Derived session key over TCP | **TLS 1.3 (via QUIC) + post-quantum hybrid X25519 + ML-KEM-768** | Forward secrecy + harvest-now-decrypt-later defense, using vetted primitives. Go 1.24 supports the hybrid KEM. |
| Auth / key agreement | PAKE (`schollz/pake`) from the code | **SPAKE2 (RFC 9382) + channel binding to the TLS session** | Keep the short-code UX; use a published-RFC balanced PAKE; bind the derived key to the TLS session so the relay fallback path cannot MITM. **The channel binding is the single most important security detail.** |
| Integrity / hashing | xxhash / imohash (non-cryptographic) | **BLAKE3** | Cryptographically strong *and* faster than xxhash; tree structure enables verified streaming + resume-from-last-verified-chunk. |
| NAT traversal | LAN: multicast UDP discovery (`schollz/peerdiscovery`). Cross-internet: the relay returns each client's public IP via the banner, peers exchange them via `TypeExternalIP` — but the discovered IPs are only used for display strings, never for any connection attempt. The only dial attempt uses RFC1918 interface addresses (always fails across the internet). **No NAT hole-punching.** | **mDNS (LAN) → ICE (`pion/ice`) hole punching → QUIC over the established socket** | This is what makes fsend actually P2P. Croc has the discovery half of NAT traversal (vestigial) but never wrote the traversal half. fsend completes it via Pion's ICE — the most battle-tested traversal lib outside the browser. Do **not** hand-roll hole punching. |
| Data path | LAN: peer-to-peer. Cross-internet: all bytes through the relay (no hole-punching). | **Direct P2P when hole punching succeeds (~70% in the wild); relay only as fallback** | The headline improvement: in symmetric-NAT-to-symmetric-NAT (the dominant cross-internet case), croc must relay; fsend can punch. |
| Server / relay | Relay opens a TCP port *range* | **One server binary behind a reverse proxy: TCP/443 (HTTPS, signaling) + UDP/443 (relay forwarding)** | Three roles in one process: signaling (code-matching), STUN-style address reflection, TURN-style relay fallback. TLS terminated by the operator's reverse proxy — server holds no certs. Leaner to self-host. |
| Distribution | Static binary via `curl \| sh`; Docker relay | **Same** | Don't change what works. |

## Honest caveat to keep in the design and the README

~70% of cross-NAT transfers hole-punch directly; ~30% (both peers behind
symmetric NAT) fall back to the relay. The accurate promise is **"direct by
default, relay when it must"** — not "never relays." Do not claim 100% relay-free.

## Architecture: two deliverables, one codebase

A single Go module (`github.com/polius/fsend`, final path TBD when repo is created)
with two binaries sharing a common internal package.

- **CLI** (`cmd/fsend`): what end users run to send/receive. Binary name
  `fsend`. Cross-compiled to many targets, shipped as static binaries via the
  install script.
- **Server** (`cmd/fsend-server`): the coordination/rendezvous component you
  (or a self-hoster) run. Binary name `fsend-server`. Does code-matching
  (signaling), STUN-style address reflection, and relay fallback. Shipped as
  a small Docker image. Needs UDP 443 (+ optional TCP 443) open; **clients
  open no ports** (outbound only — NAT creates return mappings).
- **Shared internal package** (`internal/...`): the wire protocol, SPAKE2 PAKE
  handshake, channel binding, QUIC setup, BLAKE3 chunking/verification, framing.

## Connection flow

1. Sender generates a short code in the format `xxx-xxxx-xxx`
   (e.g. `abc-defgh-jkm`); user shares it out of band. See "Code format" below.
2. Both peers contact the rendezvous server and run **SPAKE2** (RFC 9382) over
   the code to derive a shared key (the code never crosses the wire).
3. Try **mDNS** first — LAN transfers should never touch the internet.
4. For remote peers: server reflects each peer's observed public address to the
   other (STUN-style), then **ICE** gathers candidates and hole-punches.
5. Establish **QUIC** over the resulting socket. Bind the SPAKE2-derived key
   into the TLS session via an RFC 5705 exporter (channel binding).
6. Stream the file in **BLAKE3**-verified chunks; resume from the last verified
   chunk on interruption.
7. If hole punching fails, fall back to relaying QUIC through the server — which,
   thanks to channel binding, is a blind pipe that only sees ciphertext.

## Critical implementation note

Do the ICE hole punch and then run QUIC over the **same** socket. `quic-go`
supports handing it an existing `net.PacketConn` via its `Transport` type — do
*not* let quic-go open its own socket, or the punched NAT mapping is wasted.
This wiring (ICE-established conn → quic-go Transport) is the highest-risk part
of the build. Get it working first.

## Suggested Go libraries

All choices are pure Go where possible (preserves single static binary,
cross-compile to every target). cgo dependencies are explicitly called out.

- **Transport: `github.com/quic-go/quic-go`** — the only production-ready
  QUIC library in Go. 11.6k stars, used by Cloudflare, Caddy, libp2p,
  Tailscale. Active (last release June 2026). No real alternative.
- **NAT traversal + LAN discovery: `github.com/pion/ice/v4`** — single
  import, full stack. Includes `pion/stun`, `pion/turn`, and `pion/mdns/v2`
  transitively, so mDNS LAN discovery comes for free without a second
  dependency.
- **Hashing/integrity: `github.com/zeebo/blake3`** — pure Go, AVX2 + SSE4.1
  SIMD acceleration. Used in production by Storj (storage network) and
  several IPFS forks. Faster than `lukechampine.com/blake3` on typical
  consumer CPUs (AVX-512 only matters on enterprise/recent hardware).
  Last commit March 2026.
- **PAKE: SPAKE2 (RFC 9382)** via `salsa.debian.org/vasudev/gospake2`,
  vendored and pinned into our repo. See [`docs/decisions/pake.md`](docs/decisions/pake.md)
  for rationale. `schollz/pake` is out of scope.
- **TLS / PQC hybrid: Go 1.24+ `crypto/tls`** (`X25519MLKEM768`). Stdlib —
  no external dependency, and the stdlib is the only production-ready Go
  source for hybrid post-quantum key exchange today.
- **CLI scaffolding: `github.com/spf13/cobra`** (v1.10+). The de-facto
  standard for serious Go CLIs — used by kubectl, helm, gh, hugo, docker,
  podman, doctl, fly, terraform. Free shell completions (bash/zsh/fish/
  powershell), free man-page generation, polished help output. More
  boilerplate than `urfave/cli`, but the polish is worth it for a tool
  that will live in users' `PATH` for years.
- **Progress bar: `github.com/vbauerster/mpb/v8`** — used by **rclone**,
  the gold-standard file-transfer CLI in Go. Composable decorator API,
  excellent multi-bar support (needed for directory transfers where the
  user wants to see per-file progress *and* overall progress).
- **Compression (auto-detect): `github.com/klauspost/compress`** (zstd).
  The uncontested Go zstd implementation. Used by Caddy, MinIO, Cockroach.
  Last commit today, 4 open issues.
- **Clipboard: `github.com/atotto/clipboard`** — pure Go, shells out to
  `xclip`/`xsel` on Linux, `pbcopy` on macOS, native API on Windows.
  Preserves our static-binary story. Linux users without xclip/xsel
  installed see "clipboard not available" gracefully — document this in
  the README install section.
- **XDG / config paths: `github.com/adrg/xdg`** — cross-platform config
  directory resolution (XDG on Linux, `~/Library/Application Support` on
  macOS, `%APPDATA%` on Windows). Rolling our own is 30 lines but
  error-prone, especially on Windows.

## PAKE decision (summary)

**Protocol:** SPAKE2 (RFC 9382).
**Library:** `salsa.debian.org/vasudev/gospake2`, forked and vendored into
`internal/pake/spake2/`.
**Insulation:** all call sites use an `internal/pake` interface
(`Initiator` / `Responder`), so the implementation can be swapped without
touching downstream code.
**Group constants:** SPAKE2 reference `M` and `N` from RFC 9382 §4 (ed25519).
**Channel binding:** PAKE-derived key mixed into the TLS 1.3 session via an
RFC 5705 exporter — this is the load-bearing MITM defense.

Full rationale, candidate comparison, ecosystem audit, and migration path:
see [`docs/decisions/pake.md`](docs/decisions/pake.md).

## Decision documents (implementation-ready specs)

The protocol surfaces and resumable-transfer semantics live in companion
docs so this spec stays scoped to product/architecture. Each document below
is intended to be detailed enough to implement directly:

- [`docs/decisions/pake.md`](docs/decisions/pake.md) — SPAKE2 vs CPace,
  library audit, vendoring + insulation strategy
- [`docs/decisions/wire-protocol.md`](docs/decisions/wire-protocol.md) —
  bytes between peers: framing, chunk format, control vs data streams,
  error catalog, version negotiation
- [`docs/decisions/signaling-protocol.md`](docs/decisions/signaling-protocol.md) —
  HTTP endpoints between clients and `fsend-server`: session lifecycle,
  pairing, ICE candidate exchange
- [`docs/decisions/relay-protocol.md`](docs/decisions/relay-protocol.md) —
  UDP fallback path: per-datagram framing, session-token demux,
  byte-counting for the relay cap
- [`docs/decisions/resume.md`](docs/decisions/resume.md) — partial-file
  format, resume handshake, validation rules
- [`docs/decisions/directory-transfer.md`](docs/decisions/directory-transfer.md) —
  walk semantics, symlinks, modes, overwrite behavior, stdin/text
- [`docs/operations.md`](docs/operations.md) — what `fs.alzina.dev` is
  (and isn't) as a public service: no SLA, may go down, self-hosting
  is the path for guaranteed availability
- [`docs/security/threat-model.md`](docs/security/threat-model.md) —
  attackers we defend against (and don't), trust boundaries, residual
  risks, cryptographic primitives summary
- [`docs/security/privacy.md`](docs/security/privacy.md) — what the CLI
  collects (nothing), what the server sees, retention, reproducible
  builds — user-facing privacy policy
- [`docs/ux/help-text.md`](docs/ux/help-text.md) — exact `--help` output
  for both binaries plus the catalog of user-facing error messages with
  stable exit codes
- [`docs/ux/failure-states.md`](docs/ux/failure-states.md) — visual UX
  for every failure mode (pre-transfer, mid-transfer, edge cases)
- [`docs/ux/first-run.md`](docs/ux/first-run.md) — the first-invocation
  disclosure block and what we deliberately don't do
- [`docs/decisions/implementation-defaults.md`](docs/decisions/implementation-defaults.md) —
  Go version, repo layout, build matrix, release artifact naming, logging
  library, error handling style, cobra usage, Pion ICE + quic-go config,
  TLS exporter labels, codesigning posture, testing strategy

## CLI UX

The CLI deliberately drops croc's `send` verb. A single dispatch verb (`fsend`)
serves both directions; mode is inferred from the argument shape. Code phrases
match a strict regex (`^[a-hjkmnp-z]{3}-[a-hjkmnp-z]{4}-[a-hjkmnp-z]{3}$`),
which makes collisions with real filenames effectively impossible — and when
one does occur, the user is prompted rather than guessed at.

### Code format

Codes follow the `xxx-xxxx-xxx` pattern popularized by Google Meet and used
by [FileSync](https://github.com/polius/FileSync). 10 lowercase letters,
hyphenated 3-4-3.

**Alphabet:** `abcdefghjkmnpqrstuvwxyz` (23 letters). We exclude `i`, `l`,
`o` because they look like `1`, `1`, `0` when read off a screen, dictated
over a call, or written on paper. Filesync uses the full 26-letter alphabet
because they show a QR code alongside; fsend is CLI-first, so users will
actually type these — visual disambiguation matters more.

**Generation:** `crypto/rand` over the alphabet, 10 picks. Sample biased
toward zero is avoided by rejection sampling (standard `crypto/rand.Int`).

**Entropy:** log2(23¹⁰) ≈ **45 bits**. More than enough for a PAKE — even
a perfect online attacker doing 1 attempt/second would need >500,000 years
on average, and the server rate-limits new sessions per IP, killing online
guessing entirely.

**Why not croc-style word phrases (`7-banana-staple-river`)?** They're
~22 chars vs 12, harder to type, and feel whimsical rather than
professional. The Google Meet format is already familiar to most users and
fits the brand we want.

**Regex for dispatch matching:** `^[a-hjkmnp-z]{3}-[a-hjkmnp-z]{4}-[a-hjkmnp-z]{3}$`

### Send-side terminal UX

When the user runs `fsend <path>`, the CLI prints a two-block layout to
stderr: an "artifact" block (the code and the receive command the user
needs to share) and a "status" block (what the tool is doing).

This is deliberately cleaner than croc's output, which inlines `Code is:
<code>` alongside two OS-specific receive commands and a clipboard hint —
visually flat, hard to scan, redundant on macOS/Linux.

#### Lifecycle (rendered on stderr, ANSI when TTY, ASCII fallback otherwise)

**State 1 — waiting for receiver:**

```
  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm

  On the other machine, run:
      fsend abc-defgh-jkm

  ──────────────────────────────────────────────

  ✓ Code copied to clipboard
  ⠋ Waiting for receiver…
```

**State 2 — receiver connected, transfer in progress:**

```
  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm  →  Sarah's MacBook

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  ▕████████████████░░░░░░░░░░░░░░░░░░░░░░░░▏  42%   1.8 MB/s   ETA 1s
```

If the transfer falls back to the relay, the line reads:
`⚠ Relayed via fs.alzina.dev (NAT hole-punch failed)` — honest, not hidden.

**State 3 — complete:**

```
  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm  →  Sarah's MacBook

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  ✓ Sent 4.2 MB in 2.4 s  (1.75 MB/s avg)
```

### Receive-side terminal UX

When the user runs `fsend abc-defgh-jkm`, the CLI shows a confirmation
prompt with the incoming file's metadata, then a matching progress display.

**State 1 — prompt:**

```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

  Save to current directory? [Y/n]: _
```

`--yes` skips the prompt. `--out <dir>` changes the target. The peer
hostname is whatever the sender's OS reports (overridable later with a
`--name` flag if needed).

**State 1b — password prompt (only when sender used `--pass`):**

```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)  🔒 Password required

  ──────────────────────────────────────────────

  Enter password: _
```

Wrong password aborts both sides; sender sees `✗ Receiver entered wrong
password — aborted`. The receiver gets one attempt then disconnects (no
infinite-retry to discourage interactive guessing, though a 45-bit code
plus a password is far out of reach of online attacks anyway).

**State 2 — transferring:** same progress display as the sender.

### Design rules (locked)

1. **Stderr for everything visual.** Stdout stays clean for pipelines.
2. **In `--quiet`, stdout gets only the bare code** (`abc-defgh-jkm\n`) —
   no banners, no whitespace. Makes `fsend foo.pdf --quiet | pbcopy`
   trivial. All progress and status output is silenced.
3. **Clipboard copy on by default.** `--no-clipboard` disables. The "✓
   Code copied to clipboard" line is visible so users don't double-copy.
4. **Always show the data path** (direct vs relayed) — a real
   differentiator vs croc, and users learn that fsend punched through
   their NAT.
5. **Receive command shown once, OS-agnostic.** Just `fsend <code>`. Croc's
   dual Windows/macOS-Linux variants are unnecessary noise.
6. **ANSI when stderr is a TTY, ASCII fallback when piped.** Unicode
   box-drawing and spinner glyphs auto-degrade to `-`, `>`, `[ ]`.
7. **Errors go to stderr in red, exit code non-zero.** Scripts can detect
   failure with `$?`.

### Dispatch rules (locked)

| Invocation | Action |
|---|---|
| `fsend` (no args, no flags) | Print help |
| `fsend --code <code> [path...]` | **Send** (explicit code provided; if no path, send from stdin) |
| `fsend --text "..."` | **Send** the literal string as the payload (no file) |
| `fsend <code>` where `<code>` matches the code regex AND no file/dir with that name exists in CWD | **Receive** |
| `fsend <code>` where `<code>` matches the code regex AND a file/dir with that name exists in CWD | **Prompt** — `[s]end this file, or [r]eceive with this code?` |
| `fsend <path>` where `<path>` does not match the code regex | **Send** (single path) |
| `fsend <path1> <path2> ...` (2+ args) | **Send** (multi-path; codes are always single tokens) |
| `fsend -` | **Send from stdin** |
| `fsend --send <arg>` / `fsend --receive <arg>` | Force mode, skip auto-detect (scripting safety valve) |

`--code` shortcut: when `--code` is set on the command line, the operation is
unambiguously **send** — no regex check, no prompt, no auto-detect.

### Other CLI flags

**Transfer behavior:**
- `--code <code>` — provide the code phrase explicitly on send (skips random gen; implies send mode)
- `--text "<string>"` — send a literal string instead of a file (croc parity)
- `--no-compress` — force-disable compression. Useful for benchmarking; not
  needed for normal use because compression auto-detects (see below).
- `--yes` — auto-accept incoming transfers (skip receiver prompt)
- `--out <dir>` — receive into a specific directory
- `--overwrite` — overwrite existing files on receive
- `--quiet` — suppress all non-error output; in send mode, prints only the
  bare code on stdout for clean piping (`fsend foo.pdf --quiet | pbcopy`)
- `--no-clipboard` — don't auto-copy the code to the clipboard on send
  (default: copy)
- `--pass <password>` — require the receiver to enter this password
  before the transfer starts. Optional extra confirmation on top of the
  code phrase. See "Password protection" below.
- `--name <string>` — override the hostname shown to the peer (default:
  the OS-reported hostname)

**Password protection (`--pass`):** when the sender sets `--pass`, the
receiver gets prompted for it after the connection is established but
before any file bytes flow. Wrong password → both sides abort cleanly.

This is **not** a cryptographic second factor — the PAKE code already
provides strong authentication. It exists as a UX-level confirmation
defending against narrow scenarios: shoulder-surfing during code display,
accidentally sharing the code in the wrong chat, screen-share leaks
mid-meeting. The password is shared out of band (different channel from
the code) and acts as "are you the person I expect?" confirmation.

Implementation: sender sends `HMAC-SHA256(password, exporter_key)` as the
first encrypted message after the TLS handshake, where `exporter_key` is
the 32-byte output of the TLS 1.3 RFC 5705 exporter with label
`"EXPORTER-fsend-pwd-challenge"` and empty context. Receiver computes the
same value from the password the user types and compares with
`crypto/subtle.ConstantTimeCompare`. The password itself never crosses
the wire, and the exporter binding prevents replay across sessions.
See [`docs/decisions/wire-protocol.md`](docs/decisions/wire-protocol.md)
for the on-the-wire frame format (`PASSWORD_CHALLENGE` / `PASSWORD_RESPONSE`).

**Compression behavior:** the sender peeks at the first chunk of each file
and tries zstd. If the result is smaller by ≥10%, the rest of the file is
sent compressed; otherwise it's sent raw. This handles the common cases
correctly without a user-tunable flag: text/code/logs compress, while
photos/videos/zips/PDFs (already-compressed) pass through unchanged. The
receiver always decompresses based on a per-chunk header bit.

**Server configuration:**
- `--connect <host:port> [<password>]` — set the rendezvous server (and
  optional shared password). **The value is persisted** to
  `~/.config/fsend/config.json` (XDG-compliant) and used by subsequent
  invocations.
- `--connect default` — revert to the compiled-in default server
  (`fs.alzina.dev:443`).
- `--connect` (no arguments) — print the current configured server and the
  compiled-in default:

  ```
  $ fsend --connect

  Current server: relay.mycompany.com:443 (custom, set 2026-05-12)
  Default server: fs.alzina.dev:443

  To revert to the default:  fsend --connect default
  To set a new server:       fsend --connect <host:port> [password]
  ```

  If no custom server has been set, the "Current server" line reads
  `fs.alzina.dev:443 (default)`.

### Client config file

Location: `~/.config/fsend/config.json` (XDG on Linux,
`~/Library/Application Support/fsend/config.json` on macOS,
`%APPDATA%\fsend\config.json` on Windows — handled by `adrg/xdg`).

Schema:

```json
{
  "schema_version": 1,
  "server": "relay.mycompany.com:443",
  "server_password": "optional-shared-secret",
  "last_update_check": "2026-06-03T14:22:11Z",
  "latest_known_version": "0.2.0"
}
```

- Missing file or missing fields → use compiled-in defaults silently.
- Unknown `schema_version` → treat as missing, warn at debug level.
- File mode: `0600` (private to the user) when written, since it may
  contain `server_password`.

### Timeouts (locked)

| Operation | Timeout | Notes |
|---|---|---|
| Connect to rendezvous server | 10 s | Fail with helpful error pointing at `--connect` |
| Long-poll for peer pairing | 25 s per request | Auto-retries until session TTL expires (60 s total) |
| mDNS LAN discovery before falling through to internet rendezvous | 300 ms | Croc uses 200 ms; we give a bit more for reliability |
| ICE gathering | 5 s | Matches `pion/ice` defaults |
| ICE connectivity checks (total budget before relay fallback) | 15 s | If no ICE pair succeeds in this window, fall back to relay |
| Receiver prompt timeout | 30 s | If user doesn't answer, sender sees timeout and aborts |
| Password prompt timeout | 60 s | Per attempt; receiver gets one attempt |
| Mid-transfer idle (no chunks for X seconds) | 30 s | Either side aborts with `TIMEOUT` error |

### Behavior when server is unreachable

```
$ fsend report.pdf
✗ Could not reach rendezvous server fs.alzina.dev:443 (timeout after 10s).
  Check your internet connection, or set a different server with:
    fsend --connect <host:port>
  To use the default again later:
    fsend --connect default
```

Exit code: 2 (network error). `--quiet` still prints this error (to stderr).

### Update-check edge cases

- `fsend --version` triggers a synchronous update check (with 1 s timeout)
  and prints both lines:
  ```
  fsend 0.1.0 (build a1b2c3d, 2026-05-12)
  A newer version is available: 0.2.0 — https://github.com/polius/fsend/releases/latest
  ```
- During a transfer, the update check runs in a goroutine and the result is
  printed *after* the final summary line, separated by a blank line.
- `FSEND_NO_UPDATE_CHECK=1` disables; `--no-update-check` disables for the
  invocation only.

**Maintenance:**
- `--uninstall` — remove the `fsend` binary from `PATH` and the config dir
- `--version` — print version, build commit, and check for updates (see
  "Update checking" below)
- `--help`

## Update checking

On every invocation, the CLI performs a non-blocking, time-budgeted check
against the GitHub Releases API for a newer version:

- Request: `GET https://api.github.com/repos/polius/fsend/releases/latest`
- Timeout: 1 second; failures are silent (never block or annoy the user)
- Cache: result cached in `~/.config/fsend/update_cache.json` for 24 hours;
  most invocations hit the cache and do zero network work
- If a newer semver is found: print a one-line notice on stderr after the
  transfer completes (never before — must not interrupt the UX)

Opt-outs:
- `FSEND_NO_UPDATE_CHECK=1` environment variable
- `--no-update-check` flag

This is the only network call the CLI makes outside of a transfer.

## Server operation

The server (`fsend-server`) is a single static binary, shipped as a `FROM scratch`
Docker image. **TLS termination is the operator's job**, handled by a reverse
proxy (Caddy, Nginx, Traefik, cert-manager-in-k8s, etc.). fsend-server itself
speaks plain HTTP for signaling and raw UDP for the relay data path — no
certs, no ACME, no domain config baked into the binary.

This deliberately keeps the server's responsibility narrow. The operator points
a domain at their box, runs whatever reverse proxy they already use, and lets
that handle HTTPS + cert renewal.

### Zero-config by default

The standard install is one command, no flags:

```
docker run -p 443:443/udp -p 8080:8080/tcp ghcr.io/polius/fsend-server
```

That's it. The server picks sane defaults for everything and writes logs
to stdout (Docker captures them). 99% of operators never need to set
anything.

### Configuration (env vars)

When operators do need to deviate, **environment variables** are the
config surface — no operational CLI flags. Env vars compose cleanly with
Docker, Kubernetes, systemd, Fly, Compose, and every other deployment
context; flags don't.

| Env var | Default | Purpose |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | Bind address for signaling HTTP (behind reverse proxy) |
| `FSEND_UDP_ADDR` | `:443` | Bind address for relay UDP datagrams |
| `FSEND_LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `100MiB` | Hard cap on bytes per relayed session |
| `FSEND_MAX_SESSIONS_PER_IP` | `5` | Concurrent sessions per source IP |
| `FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN` | `30` | New-session rate limit per IP |
| `FSEND_SESSION_IDLE_TIMEOUT` | `60s` | Drop sessions with no traffic for this long |

Setting any limit to `0` disables that specific check. Limits apply only
to the relay fallback path; signaling and STUN-style reflection are
cheap and uncapped.

### CLI flags (only the basics)

The binary itself has just two flags, both universal:

- `--version` — print version and exit
- `--help` — print usage and exit

No operational flags. Everything operational is an env var.

### Topology

```
   Clients ── HTTPS ──► Reverse proxy (Caddy/Nginx) ── HTTP ──► fsend-server
                       (TLS termination + cert mgmt)            (signaling)

   Clients ── raw UDP datagrams (end-to-end QUIC ciphertext) ──► fsend-server
                                                                  (relay)
```

- TCP/443: operator's reverse proxy terminates HTTPS and forwards plain HTTP
  to fsend-server's HTTP listener. This carries signaling only — small JSON
  messages. The reverse proxy uses any cert management the operator likes.
- UDP/443: forwards directly to fsend-server's UDP listener. The data
  flowing through it is end-to-end QUIC ciphertext between Alice and Bob;
  the server never enters this TLS session, so no cert is needed at this
  layer.

A minimal `docker-compose.yml` for self-hosters ships in the repo,
combining `fsend-server` + Caddy (which gets Let's Encrypt certs for free).

## Suggested build order (de-risk hardest parts first)

1. **LAN-only MVP**: QUIC + SPAKE2 auth + channel binding + BLAKE3 verified
   transfer, peers discovered via mDNS, no internet rendezvous. Proves the
   secure core — the handshake, the binding, the verified streaming.
2. Add the rendezvous server + STUN-style reflection + ICE hole punching for
   remote peers.
3. Add relay fallback for symmetric-NAT cases.
4. Add the `curl | sh` installer + GitHub Actions release matrix + the
   `FROM scratch` server Docker image.

## Distribution detail

- CLI: GitHub Actions matrix builds static binaries per target → GitHub Releases
  → `install.sh` detects OS/arch (`uname -s`/`uname -m`), downloads the matching
  asset, drops it on `PATH`. Publish checksums for manual verification too.
  Default rendezvous baked in: `fs.alzina.dev:443` (overridable via `--connect`).
- Server: multi-stage Dockerfile, final stage `FROM scratch` with just the
  static binary → push to a registry → operators `docker run` / compose. The
  repo ships a reference `docker-compose.yml` that wires `fsend-server` behind
  Caddy for automatic Let's Encrypt cert management. No cert files inside the
  fsend-server container.

## Versioning + compatibility promise

**Semantic versioning** (semver 2.0). Until v1.0.0, breaking changes can
happen on minor bumps with a clear changelog note. After v1.0.0:

- **PATCH** (v1.0.X) — bug fixes only. Wire-compatible with all
  v1.0.\* versions in both directions.
- **MINOR** (v1.X.0) — new features, no breaking changes. New CLI flags,
  new optional wire-protocol frame types. Old clients still talk to new
  servers and vice versa.
- **MAJOR** (vX.0.0) — breaking changes. Wire-protocol version byte may
  bump. Backward compat may be dropped after one major-version
  deprecation window.

**Client ↔ server compatibility:** any released `fsend` CLI must work
with any released `fsend-server` of the same major version. Self-hosters
do not need to upgrade their server when users upgrade their CLI within
a major.

**Wire protocol version (see `docs/decisions/wire-protocol.md`):**
- v1 protocol byte = `0x01`, ships from v0.1.0 onward
- Bumps require a major version on the user-visible side
- Both peers must agree on the version; mismatch surfaces as
  `UNSUPPORTED_VERSION` with a clear "upgrade fsend" message

**Release cadence:** no fixed schedule. Patch releases as fixes warrant,
minor releases when meaningful features land, major releases only when
strictly necessary.

**Deprecation policy:** any flag, env var, or wire-protocol element
marked deprecated stays functional for at least one minor version after
the deprecation, with a stderr warning to users who hit it.

**Signed releases:** every release tag is signed with the project's
cosign key; signatures published to the Rekor transparency log.
SHA-256 checksums in `checksums.txt`, also signed. See
[`docs/security/privacy.md`](docs/security/privacy.md) for
reproducible-build instructions.
