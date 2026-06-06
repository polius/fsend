# Usage

Complete reference for the `fsend` CLI and its `server` subcommand.

- [Usage](#usage)
  - [Quick reference](#quick-reference)
  - [Sending](#sending)
    - [Sender flags](#sender-flags)
    - [Sender examples](#sender-examples)
  - [Receiving](#receiving)
    - [Receiver flags](#receiver-flags)
    - [Receiver examples](#receiver-examples)
  - [Choosing a server (`--connect`)](#choosing-a-server---connect)
  - [Environment variables](#environment-variables)
  - [Shared flags](#shared-flags)
  - [`fsend server` reference](#fsend-server-reference)
    - [Server environment variables (all optional)](#server-environment-variables-all-optional)
    - [Running the server](#running-the-server)
  - [Exit codes](#exit-codes)
  - [Codes](#codes)

---

## Quick reference

| Goal | Command |
|---|---|
| Send a file | `fsend report.pdf` |
| Send a folder | `fsend ./project` |
| Send multiple files | `fsend a.txt b.txt c.txt` |
| Send from stdin | `cat file.bin \| fsend` |
| Send a literal string | `fsend --text "hello world"` |
| Receive | `fsend abc-defg-jkm` |
| Receive without prompt | `fsend --yes abc-defg-jkm` |
| Use your own server | `fsend --connect fs.example.com:443` |
| Reset to default server | `fsend --connect default` |
| Show current server | `fsend --connect` |
| Help / version | `fsend --help` / `fsend --version` |

`fsend` decides whether to send or receive automatically:

- An argument that **looks like a code** (three letter-groups separated by `-`,
  using the alphabet `abcdefghjkmnpqrstuvwxyz`) → receive mode.
- Anything else → send mode.

If an argument is *both* a valid code **and** the name of a file in your
current directory, `fsend` detects the collision and asks
`[s]end this file, or [r]eceive with this code?` before doing anything. You'll
never silently send the wrong thing.

For scripts and other non-interactive runs where there's no terminal to answer
that prompt, pass `--send` or `--receive` to commit up front.

---

## Sending

```text
fsend [file|dir]... [flags]
```

You'll see a code like `abc-defg-jkm`. Share it with the receiver.

When stdin is piped or redirected (`echo hi | fsend`, `fsend < file.bin`),
`fsend` sends what's coming in — no positional argument needed.

### Sender flags

| Flag | Purpose |
|---|---|
| `--text <string>` | Send a literal string instead of a file. |
| `--pass <password>` | Require the receiver to supply this password before the transfer starts. Pass `--pass` with no value to be prompted interactively — a fresh random password is suggested and accepted with Enter. See [Environment variables](#environment-variables) for `FSEND_PASS`. |
| `--exclude <pattern>` | Glob patterns to skip when bundling a directory. Repeatable or comma-separated, e.g. `--exclude '*.log,node_modules'`. |
| `--name <hostname>` | Override the hostname shown to the peer in the confirmation prompt. |

### Sender examples

```sh
# Single file
fsend report.pdf

# Multiple files (received as separate files)
fsend a.txt b.txt c.txt

# Folder — bundled into a single archive, unpacked on the other side
fsend ./project

# Folder, skipping noise
fsend ./project --exclude 'node_modules,*.log,.git'

# From stdin (no file on disk)
tar c ./build | fsend
fsend < big.iso

# A literal string (great for sharing snippets)
fsend --text "the wifi password is hunter2"

# Gate the transfer with a password (visible in `ps`)
fsend --pass swordfish ./secret.tar.gz

# Or bare --pass after the file to be prompted, with a fresh random
# default you can accept by pressing Enter
fsend ./secret.tar.gz --pass
```

---

## Receiving

```text
fsend <code> [flags]
```

By default `fsend` shows the sender's hostname and what they're sending, then
asks you to confirm. Pass `--yes` to skip the prompt.

### Receiver flags

| Flag | Purpose |
|---|---|
| `--yes` | Auto-accept the incoming transfer. |
| `--out <dir>` | Receive into this directory (created if missing). Default: current working directory. |
| `--overwrite` | Replace existing files instead of failing with `E013`. |
| `--pass <password>` | Supply the sender's password non-interactively. See `FSEND_PASS`. |

### Receiver examples

```sh
# Interactive — prompts before accepting
fsend abc-defg-jkm

# No prompt, save into ~/Downloads/incoming
fsend --yes --out ~/Downloads/incoming abc-defg-jkm

# Overwrite anything already there
fsend --yes --overwrite abc-defg-jkm

# Password-gated transfer, supplied via env var (won't appear in `ps`)
FSEND_PASS=swordfish fsend --yes abc-defg-jkm
```

If you cancel a transfer mid-flight, a `.fsend-partial` sidecar is kept and
the next run resumes from where it left off. If the sender's file has
changed since then, fsend discards the stale sidecar automatically and
asks you to re-run — the next attempt fetches a fresh copy.

---

## Choosing a server (`--connect`)

The default pairing server is `fs.alzina.dev` — best-effort, free, and
not guaranteed. You can switch at any time:

```sh
# Switch (persisted to your config — set once per machine)
fsend --connect fs.example.com:443

# Switch and provide an admin password (for private/protected servers)
fsend --connect fs.example.com:443 mySecret

# Reset to the public default
fsend --connect default

# Show what's currently configured
fsend --connect
```

Config is stored at:

- macOS / Linux: `~/.config/fsend/config.toml`
- Windows: `%APPDATA%\fsend\config.toml`

See [Self-hosting](README.md#self-hosting) in the README if you want to run
your own server.

---

## Environment variables

All optional — the equivalent flag always wins if both are set.

| Variable | Equivalent flag | Purpose |
|---|---|---|
| `FSEND_PASS` | `--pass` | Either side: supply the password out-of-band so it stays out of shell history and `ps` output. |

---

## Shared flags

These work in both directions:

| Flag | Purpose |
|---|---|
| `--quiet` | Suppress non-error output. The transfer still happens; only errors print. |
| `--debug` | Verbose logging to stderr. Use this when filing an issue. |
| `--uninstall` | Remove the `fsend` binary and `~/.config/fsend`. Use `--yes` to skip the confirmation prompt. |
| `--help` / `-h` | Show inline help. |
| `--version` / `-v` | Print version and build info. |

---

## `fsend server` reference

The same `fsend` binary runs as the pairing + relay server when invoked
with the `server` subcommand. It has no positional arguments and only
one server-specific flag (`--health-check`); everything else is
configured via environment variables.

```text
fsend server                 Run the server
fsend server --help          Show help
fsend server --health-check  Probe /v1/health and exit 0 if healthy
fsend --version              Print version (shared with the CLI)
```

### Server environment variables (all optional)

| Variable | Default | Notes |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | TCP signaling listener. |
| `FSEND_UDP_ADDR` | `:443` | UDP relay listener. |
| `FSEND_PUBLIC_ADDR` | = `FSEND_UDP_ADDR` | `host:port` clients should dial for relay. Must be set explicitly when the public address differs from the bind address (NAT, dev box, non-default port). |
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FSEND_MAX_SESSIONS_PER_IP` | `5` | Concurrent sessions allowed per client IP. |
| `FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN` | `30` | Rate limit on new session creation. |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `100MiB` | Per-session relay cap. Accepts `500m`, `1GiB`, `104857600`, etc. |
| `FSEND_SESSION_IDLE_TIMEOUT` | `60s` | Go duration: `30s`, `5m`, `1h`. |

### Running the server

```sh
# Quick local run (debug logs, non-standard ports — see DEVELOPMENT.md)
FSEND_HTTP_ADDR=":18080" \
FSEND_UDP_ADDR=":18443" \
FSEND_PUBLIC_ADDR="127.0.0.1:18443" \
FSEND_LOG_LEVEL=debug \
fsend server

# Docker (zero-config, public ports)
docker run -p 443:443/udp -p 8080:8080/tcp poliuscorp/fsend

# Full stack with HTTPS + Let's Encrypt
# See deploy/compose/docker-compose.yml
```

---

## Exit codes

Stable from v0.1.0 onward. `0` means success; non-zero codes map to a
specific failure.

| Code  | When it happens |
|-------|-----------------|
| `0`   | Success (or non-fatal warning, e.g. `E016` config corrupted). |
| `1`   | `E001` — server unreachable. |
| `2`   | `E002` — code not found. |
| `3`   | `E003` — code already claimed by another receiver. |
| `4`   | `E004` — invalid code format. |
| `6`   | `E006` — receiver declined the transfer. |
| `7`   | `E007` — session expired on the server before a receiver paired. |
| `8`   | `E008` — disk full. |
| `9`   | `E009` — could not write the target file. |
| `10`  | `E010` — could not read the source file. |
| `11`  | `E011` — transfer completed but hash didn't verify. |
| `12`  | `E012` — path traversal rejected (security check). |
| `13`  | `E013` — target file exists, use `--overwrite`. |
| `14`  | `E014` — could not connect to peer, even via relay. |
| `15`  | `E015` — protocol error (incompatible versions). |
| `17`  | `E017` — rate limited. |
| `18`  | `E018` — default server retired. |
| `19`  | `E019` — source file changed; stale partial auto-discarded, re-run to fetch a fresh copy. |
| `20`  | `E020` — transient transfer failure (retries exhausted). |
| `21`  | `E021` — wrong password. |
| `22`  | `E022` — peer authentication failed (code mismatch or tampering). |
| `23`  | `E023` — relay's per-session byte cap reached. |
| `24`  | `E024` — invalid usage (bad flag, bad arg shape, conflicting modes). |
| `25`  | `E025` — source file or directory not found. |
| `27`  | `E027` — could not open the local-network listener (port in use, mDNS init failed). |
| `99`  | `E099` — unexpected error. Run with `--debug` and file an issue. |
| `130` | `E026` — cancelled by user (Ctrl-C / SIGTERM). |

---

## Codes

Codes look like `abc-defg-jkm`: three letter-groups (3-4-3) joined by `-`,
drawn from the 23-letter alphabet `abcdefghjkmnpqrstuvwxyz`. The
ambiguous-looking letters (`i`, `l`, `o`) are excluded. They are:

- **One-shot** — once claimed by a receiver, the same code can't be used again.
- **Short-lived** — codes expire on the server after 60 seconds if unclaimed.
- **Not the encryption key** — the code authenticates a SPAKE2 handshake;
  the actual session key is derived from that handshake plus TLS 1.3 channel
  binding, so the code itself never traverses the wire in the clear.
- **Always system-generated** — `fsend` picks the code for each transfer.
  If you need a persistent shared secret across many transfers (e.g. for
  scripts or kiosks), use `--pass` instead.
