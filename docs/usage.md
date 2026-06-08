# Usage

Reference for the `fsend` CLI and its `server` subcommand. For inline
help, run `fsend --help` or `fsend server --help`.

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

`fsend` picks send or receive automatically — a `abc-defg-jkm` shape
means receive, anything else means send. If an arg is both a valid code
and a real path, you'll be asked. Use `--send` / `--receive` to commit
up front in scripts.

## Sending

```text
fsend [file|dir]... [flags]
```

Share the code (`abc-defg-jkm`) with the receiver. Piped stdin is sent
without a positional argument.

| Flag | Purpose |
|---|---|
| `--text <string>` | Send a literal string instead of a file. |
| `--pass <password>` | Require the receiver to supply this password. Bare `--pass` prompts interactively with a random default. Also `FSEND_PASS`. |
| `--exclude <glob,…>` | Skip entries when bundling a directory. |
| `--name <hostname>` | Override the hostname shown to the peer. |

```sh
fsend report.pdf
fsend ./project --exclude 'node_modules,*.log,.git'
tar c ./build | fsend
fsend --text "the wifi password is hunter2"
fsend ./secret.tar.gz --pass         # prompts with a random default
```

## Receiving

```text
fsend <code> [flags]
```

By default the receiver sees the sender's hostname and what they're
sending, then confirms. Pass `--yes` to skip.

| Flag | Purpose |
|---|---|
| `--yes` | Auto-accept. |
| `--out <dir>` | Receive into this directory (created if missing). Default: cwd. |
| `--overwrite` | Replace existing files instead of failing with `E013`. |
| `--pass <password>` | Supply the sender's password non-interactively. Also `FSEND_PASS`. |

```sh
fsend abc-defg-jkm
fsend --yes --out ~/Downloads/incoming abc-defg-jkm
FSEND_PASS=swordfish fsend --yes abc-defg-jkm
```

Cancelled transfers leave a `.fsend-partial` sidecar and resume on the
next run. If the source has changed since, the sidecar is discarded
automatically.

## Choosing a server (`--connect`)

The default pairing server is `fsend.alzina.dev` — best-effort, free,
not guaranteed. Switch any time:

```sh
fsend --connect fs.example.com:443           # persisted
fsend --connect fs.example.com:443 mySecret  # password-gated server
fsend --connect default                      # back to the public default
fsend --connect                              # show current
```

Config is at `~/.config/fsend/config.toml` (macOS/Linux) or
`%APPDATA%\fsend\config.toml` (Windows). See
[Self-hosting](self-hosting.md) to run your own.

## Shared flags

| Flag | Purpose |
|---|---|
| `--quiet` | Suppress non-error output. |
| `--debug` | Verbose logging to stderr. Also `FSEND_DEBUG=1`. |
| `--uninstall` | Remove the binary and `~/.config/fsend`. `--yes` skips confirmation. |
| `--help` / `-h` | Show inline help. |
| `--version` / `-v` | Print version. |

## Environment variables

| Variable | Equivalent flag | Purpose |
|---|---|---|
| `FSEND_PASS` | `--pass` | Supply the transfer password out-of-band. |
| `FSEND_DEBUG` | `--debug` | `1` enables verbose stderr logging. |

Flags always win when both are set.

## `fsend server`

Same binary, run with the `server` subcommand. Configured entirely via
environment variables.

```text
fsend server                 Run the server
fsend server --health-check  Probe /v1/health (exit 0 if healthy)
fsend server --help          Show help
```

### Server environment variables

| Variable | Default | Notes |
|---|---|---|
| `FSEND_HTTP_ADDR` | `:8080` | TCP signaling listener. |
| `FSEND_UDP_ADDR` | `:443` | UDP relay listener. |
| `FSEND_PUBLIC_ADDR` | = `FSEND_UDP_ADDR` | `host:port` clients dial for relay. Set when public ≠ bind (NAT, dev box). |
| `FSEND_SERVER_PASSWORD` | _(unset)_ | Shared secret. When set, every endpoint except `/v1/health` requires it. Clients pass it via `fsend --connect <host> <password>`. |
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error`. |
| `FSEND_MAX_SESSIONS_PER_IP` | `5` | Concurrent sessions per client IP. |
| `FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN` | `30` | New-session rate limit. |
| `FSEND_MAX_RELAY_BYTES_PER_SESSION` | `100MiB` | Accepts `500m`, `1GiB`, `104857600`. |
| `FSEND_SESSION_IDLE_TIMEOUT` | `60s` | Go duration. |

```sh
docker run -p 443:443/udp -p 8080:8080/tcp poliuscorp/fsend
```

Internet-exposed deployments need a TLS-terminating reverse proxy in
front of `:8080` — file data on UDP/443 is already end-to-end encrypted,
but the HTTP pairing channel carries share codes and bearer tokens in
plaintext. See [`deploy/compose/`](../deploy/compose/) for a ready-made
Caddy + Docker stack.

## Exit codes

Stable from v0.1.0. `0` means success; non-zero codes map to a specific
failure.

| Code  | When it happens |
|-------|-----------------|
| `0`   | Success (or non-fatal warning, e.g. `E016` config corrupted). |
| `1`   | `E001` — server unreachable. |
| `2`   | `E002` — code not found. |
| `3`   | `E003` — code already claimed by another receiver. |
| `4`   | `E004` — invalid code format. |
| `6`   | `E006` — receiver declined the transfer. |
| `7`   | `E007` — session expired before a receiver paired. |
| `8`   | `E008` — disk full. |
| `9`   | `E009` — could not write the target file. |
| `10`  | `E010` — could not read the source file. |
| `11`  | `E011` — transfer completed but hash didn't verify. |
| `12`  | `E012` — path traversal rejected. |
| `13`  | `E013` — target file exists; use `--overwrite`. |
| `14`  | `E014` — could not connect to peer, even via relay. |
| `15`  | `E015` — protocol error (incompatible versions). |
| `17`  | `E017` — rate limited. |
| `18`  | `E018` — default server retired. |
| `19`  | `E019` — source file changed; stale partial discarded, re-run. |
| `20`  | `E020` — transient transfer failure (retries exhausted). |
| `21`  | `E021` — wrong transfer password. |
| `22`  | `E022` — peer authentication failed (code mismatch or tampering). |
| `23`  | `E023` — relay's per-session byte cap reached. |
| `24`  | `E024` — invalid usage (bad flag, bad arg shape). |
| `25`  | `E025` — source file or directory not found. |
| `27`  | `E027` — could not open local-network listener (port in use, mDNS init failed). |
| `28`  | `E028` — pairing server requires a password (missing or wrong). |
| `99`  | `E099` — unexpected error. Run with `--debug` and file an issue. |
| `130` | `E026` — cancelled by user (Ctrl-C / SIGTERM). |

## Share codes

Codes look like `abc-defg-jkm`: three groups (3-4-3) from the alphabet
`abcdefghjkmnpqrstuvwxyz` (the ambiguous `i`, `l`, `o` are excluded).
They're one-shot, expire on the server after one hour if unclaimed (and
ten minutes after a receiver pairs), and are always system-generated.
For persistent secrets across many transfers, use `--pass` instead.
For the security model, see [Security](security.md).
