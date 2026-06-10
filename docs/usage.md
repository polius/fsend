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

`fsend` picks send or receive automatically — an `abc-defg-jkm` shape
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
fsend --connect fs.example.com:443,mySecret  # password-gated server
fsend --connect default                      # back to the public default
fsend --connect                              # show current
```

Config is at `~/.config/fsend/config.json` (Linux),
`~/Library/Application Support/fsend/config.json` (macOS), or
`%LOCALAPPDATA%\fsend\config.json` (Windows). See
[Self-hosting](self-hosting.md) to run your own.

## Shared flags

| Flag | Purpose |
|---|---|
| `--send` / `--receive` | Force mode instead of auto-detecting from the argument. Mutually exclusive; handy in scripts. |
| `--quiet` | Suppress non-error output. |
| `--debug` | Verbose logging to stderr. Also `FSEND_DEBUG=1`. |
| `--uninstall` | Remove the binary and the fsend config directory (see [`--connect`](#choosing-a-server---connect) for the per-OS path). `--yes` skips confirmation. |
| `--help` / `-h` | Show inline help. |
| `--version` / `-v` | Print version. |

## Environment variables

| Variable | Equivalent flag | Purpose |
|---|---|---|
| `FSEND_PASS` | `--pass` | Supply the transfer password out-of-band. |
| `FSEND_DEBUG` | `--debug` | `1` enables verbose stderr logging. |
| `FSEND_NO_UPDATE_CHECK` | — | `1` disables the once-a-day check for a newer release. |

Flags always win when both are set.

After a successful transfer, fsend checks GitHub at most once a day for a
newer release and, if one exists, prints a one-line upgrade hint. The
check is skipped when output is piped or `--quiet` is set, and can be
turned off entirely with `FSEND_NO_UPDATE_CHECK=1`.

## `fsend server`

Same binary, run with the `server` subcommand. Configured entirely via
environment variables.

```text
fsend server                 Run the server
fsend server --health-check  Probe /v1/health (exit 0 if healthy)
fsend server --help          Show help
```

### Server environment variables

Full table with defaults, units, and notes:
[Self-hosting → Configuration](self-hosting.md#configuration).

Quick LAN-only run (no TLS — local testing only):

```sh
docker run -p 443:443/udp -p 8080:8080/tcp poliuscorp/fsend server
```

Internet-exposed deployments need a TLS-terminating reverse proxy in
front of `:8080` — file data on UDP/443 is already end-to-end encrypted,
but the HTTP pairing channel carries share codes and bearer tokens in
plaintext. See [Self-hosting](self-hosting.md) for the ready-made
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
| `7`   | `E007` — code timed out before anyone received. |
| `8`   | `E008` — disk full. |
| `9`   | `E009` — could not write the target file. |
| `10`  | `E010` — could not read the source file. |
| `11`  | `E011` — file arrived corrupted; hash didn't match. |
| `12`  | `E012` — path traversal rejected. |
| `13`  | `E013` — target file exists; use `--overwrite`. |
| `14`  | `E014` — could not reach the other side, even via the fallback relay. |
| `15`  | `E015` — sender and receiver are on incompatible fsend versions. |
| `17`  | `E017` — rate limited. |
| `18`  | `E018` — default server retired. |
| `19`  | `E019` — source file changed; incomplete download discarded, re-run. |
| `20`  | `E020` — transient transfer failure (retries exhausted). |
| `21`  | `E021` — wrong transfer password. |
| `22`  | `E022` — could not verify the other side (code mismatch or tampering). |
| `23`  | `E023` — server-set transfer-size limit reached. |
| `24`  | `E024` — invalid usage (bad flag, bad arg shape). |
| `25`  | `E025` — source file or directory not found. |
| `27`  | `E027` — could not open the local-network port (port in use, mDNS init failed). |
| `28`  | `E028` — pairing server requires a password (missing or wrong). |
| `29`  | `E029` — server closed the connection because no data was flowing. |
| `30`  | `E030` — `fsend server` could not start (port in use or no permission to bind). |
| `99`  | `E099` — unexpected error. Run with `--debug` and file an issue. |
| `130` | `E026` — cancelled by user (Ctrl-C / SIGTERM). |

`5` and `16` are intentionally absent: `E005` is unused, and `E016`
(corrupt config) is a non-fatal warning that exits `0`.

## Share codes

Codes look like `abc-defg-jkm` — three groups (3-4-3) from a 23-letter
alphabet (`i`, `l`, `o` are excluded for legibility). One-shot,
system-generated, server-side TTL.

To require a password before the receiver can download, add `--pass`
when sending. For the full model — entropy, TTL, rate-limiting, channel
binding — see [Security → Share codes](security.md#share-codes).
