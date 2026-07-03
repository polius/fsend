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

## Your first transfer

Two machines, one command each. **On the sender:**

```console
$ fsend report.pdf
  Sending report.pdf  ·  1 file  ·  2.4 MB

  On the other machine, run:

      fsend abc-defg-jkm
```

Share that code (`abc-defg-jkm`) with the receiver any way you like —
chat, email, out loud. **On the receiver**, run it; fsend shows who's
sending and what, then asks before writing anything:

```console
$ fsend abc-defg-jkm
  Incoming from mbp.local  ·  direct

      report.pdf  ·  1 file  ·  2.4 MB

  Save to ./? [Y/n] y
  Saved report.pdf to ./
```

The file lands in the current directory (use `--out <dir>` to change
that, or `--yes` to skip the prompt). That's the whole flow — everything
below is detail.

## Sending

```text
fsend [file|dir]... [flags]
```

Share the code (`abc-defg-jkm`) with the receiver. Piped stdin is sent
without a positional argument.

| Flag | Purpose |
|---|---|
| `--text <string>` | Send a literal string instead of a file. The receiver prints it to stdout; nothing is saved to disk. To keep it: `fsend <code> > note.txt`. |
| `--password[=<password>]` | Require the receiver to supply this password. Bare `--password` prompts interactively with a random default; supply it inline as `--password=<password>`. Also `FSEND_PASSWORD`. |
| `--exclude <glob,…>` | Skip entries when bundling a directory. |
| `--preview` | List what would be sent as CSV (`path,size`) and exit — no code, no transfer. Redirect with `> files.csv` to inspect. |

```sh
fsend report.pdf
fsend ./project --exclude 'node_modules,*.log,.git'
tar c ./build | fsend
fsend --text "the wifi password is hunter2"
fsend ./secret.tar.gz --password         # prompts with a random default
```

### Symlinks

fsend **follows** symlinks: it sends what the link points to, so the
receiver gets a real file — not a link that dangles on their machine. A
folder containing

```text
report.pdf
latest -> report.pdf      # a symlink
```

arrives with `latest` as a **real copy** of `report.pdf` (both files land —
the link's bytes are sent too). This works even when the target is outside
the folder you're sending.

A symlink whose target is missing or cyclic stops the send with `E036` —
fix it, or skip it with `--exclude`.

## Receiving

```text
fsend <code> [flags]
```

By default the receiver sees the sender's hostname and what they're
sending, then confirms. Pass `--yes` to skip.

| Flag | Purpose |
|---|---|
| `--yes` | Auto-accept. |
| `--out <dir>` | Receive into this directory (created if missing, like `mkdir -p`). Default: cwd. |
| `--out -` | Stream the payload to stdout instead of saving a file (single file, text, or piped stream — not directories). Retries are disabled: emitted bytes can't be rewound. |
| `--overwrite` | Replace existing files whose contents **differ**. Without it they're kept and the receiver exits `E013`. (Identical files are skipped either way.) |
| `--manifest <file>` | After receiving, write a CSV record (`path,size,status`) to `<file>` — what fsend did with each file (`new` / `identical` / `overwritten` / `kept` / `resumed`). |
| `--checksum` | Decide what's already present by hashing **contents** (BLAKE3) instead of comparing size + mtime — like rsync's `-c`. See [below](#when-a-file-already-exists). |
| `--password[=<password>]` | Supply the sender's password non-interactively as `--password=<password>`. Also `FSEND_PASSWORD`. |

```sh
fsend abc-defg-jkm
fsend --yes --out ~/Downloads abc-defg-jkm
fsend --yes --out - abc-defg-jkm > dump.sql   # pipe-to-pipe with the sender's `… | fsend`
FSEND_PASSWORD=swordfish fsend --yes abc-defg-jkm
```

### Password attempts

When the sender required a password and the receiver didn't supply one,
the receiver is prompted interactively and gets **3 attempts**. A wrong
password supplied non-interactively (`--password=<password>` or
`FSEND_PASSWORD`) aborts the transfer **on the first try** and burns the
code — with no human present to retry, failing fast beats hanging a
script. The sender has to run again for a fresh code.

### When a file already exists

Re-send a folder and fsend only transfers what's new or changed. How it
decides:

- **Default (fast):** fsend matches files by name, then treats one with
  the same **size and modification time** as identical and skips it
  without reading it.
- **`--checksum` (thorough):** compares the file's **contents** (a BLAKE3
  hash) instead. Slower — it reads the files already on disk — but it
  catches a file that changed in place without its size or timestamp
  changing.

When a file *does* differ, fsend keeps your local copy by default
(protecting your edits) and exits `E013`; pass `--overwrite` to replace
it. Byte-identical files are always skipped silently. The accept prompt shows
this breakdown (`N new · M up to date · K differ`) before you confirm, and
`--manifest <file>` records the exact per-file outcome after a receive.

### Resuming an interrupted transfer

If a transfer is interrupted, fsend keeps what it already received in a
`.fsend-partial` file and continues from there next time — only the
missing bytes transfer.

While the sender's fsend is **still running**, just rerun the same
`fsend <code>` — the sender re-registers the code and the transfer
resumes. Once the sender has exited, codes are **one-shot** and resuming
is a fresh send with a new code:

1. **Sender** runs the original command again. fsend issues a *new* code.
2. **Share the new code** with the receiver (the old one no longer works).
3. **Receiver** runs the new code **in the same directory** as before.
   fsend finds the `.fsend-partial` file and picks up where it stopped.

If the source file changed in the meantime, the partial is discarded and
the file transfers from scratch — so you never resume onto stale data.

## Scripting

`--quiet` makes fsend scriptable: on send it prints **only the share code**
to stdout (progress, prompts, and summaries go to stderr), then blocks
until a receiver connects. Capture the code from stdout:

```sh
fsend file1.txt --quiet > code.txt    # prints the code, then waits
```

On receive, pair `--quiet` with `--yes` (nothing else can answer the
prompt) and pass any password via `FSEND_PASSWORD`:

```sh
FSEND_PASSWORD=swordfish fsend "$(cat code.txt)" --quiet --yes --out ~/incoming
```

### JSON output (`--json`)

`--json` turns stdout into NDJSON — one JSON object per line, nothing
else. Human output (progress, prompts, summaries) stays on stderr, so
`--json` composes with or without `--quiet`. Exit codes are unchanged.

Two events exist. The **sender** emits the share code as soon as it is
issued, then a final summary; the **receiver** emits only the summary:

```json
{"v":1,"event":"code","code":"abc-defg-jkm"}
{"v":1,"event":"done","ok":true,"role":"sender","exit":0,"bytes_total":2100000,"bytes_moved":2100000,"files_sent":1,"files_skipped":0,"files_kept":0,"duration_ms":312,"route":"local"}
```

Fields of `done`:

| Field | Meaning |
| --- | --- |
| `ok` / `error` / `exit` | Outcome; `error` is the catalog code (e.g. `"E013"`) and matches the exit code table below |
| `role` | `"sender"` or `"receiver"`; omitted when the command fails before either path is entered (e.g. a flag-parse error) |
| `bytes_total` | Per role: the sender reports the full offered size; the receiver reports the bytes it accepted for writing (up-to-date and kept files count 0) — the two sides can differ for the same session |
| `bytes_moved` | Bytes that crossed the wire this run (a re-send of unchanged files moves 0) |
| `files_sent`, `files_skipped`, `files_kept` | Sender counts (`files_skipped` = receiver-declined for an unknown reason — old receivers don't report the distinction) |
| `files_saved`, `files_up_to_date`, `files_kept` | Receiver counts |
| `duration_ms`, `route` | Timed from the first byte; `route` is `local`, `direct`, or `relay` |
| `dir` | Receiver: absolute directory files landed in |
| `text` | Receiver: a `--text` payload rides here instead of raw stdout |

On a failure before any summary exists (bad code, unreachable server),
the `done` event carries only `ok`/`error`/`exit` — plus `role` when the
failure happened after a send or receive path was entered. Fields are
only ever **added** to this schema; `v` bumps on the first breaking
change.

`--json` is rejected with `--preview` (already CSV) and with `--out -`
(stdout carries the received bytes).

## Chaining through a middle machine

If the sender and receiver can't reach each other but a third machine can
reach both, splice two transfers on it: `--out -` streams a received
payload to stdout, and a piped `fsend` sends stdin. The bytes flow
`A → B → C` through the pipe — B never writes them to disk, and each hop
is separately paired and encrypted:

```sh
fsend file.bin                          # A: prints code A
fsend <codeA> --out - --yes | fsend     # B: relays A's stream; prints code B
fsend <codeB> --out - > file.bin        # C: saves it
```

The pipe carries one raw stream — no names or folders — so redirect to the
name you want on C, and for a directory wrap it in `tar` (`tar c ./dir | fsend`
on A, `fsend <code> --out - | tar x` on C). Because B decrypts each hop to
re-send it, only chain through a machine you trust.

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
| `--name <string>` | Override the device name shown to the peer (default: hostname) — the sender's name in the receiver's `Incoming from …` line, the receiver's name in the sender's `Receiver connected` line. |
| `--quiet` | Suppress non-error output. On send, prints **just the share code** to stdout (see [Scripting](#scripting)); on receive, requires `--yes`. |
| `--debug` | Verbose logging to stderr. Also `FSEND_DEBUG=1`. |
| `--update` | Update fsend to the latest release by re-running the installer in place. Reports when already up to date. |
| `--uninstall` | Remove the binary and **recursively delete** the fsend config directory (see [`--connect`](#choosing-a-server---connect) for the per-OS path). `--yes` skips confirmation. |
| `--help` / `-h` | Show inline help. |
| `--version` / `-v` | Print version. |

## Environment variables

| Variable | Equivalent flag | Purpose |
|---|---|---|
| `FSEND_PASSWORD` | `--password` | Supply the transfer password out-of-band. |
| `FSEND_DEBUG` | `--debug` | Set to `1` (any value except `0`/`false`) for verbose stderr logging. |
| `FSEND_NO_UPDATE_CHECK` | — | Set to `1` (any value except `0`/`false`) to disable the once-a-day check for a newer release. |
| `NO_COLOR` | — | Set to any non-empty value to disable ANSI colors ([no-color.org](https://no-color.org)). |
| `FORCE_COLOR` | — | Set to `1` (any value except `0`/`false`) to force ANSI colors on, even when output isn't a terminal. Colors only — spinners and progress bars still require a terminal. |
| `TERM` | — | `TERM=dumb` disables all ANSI escapes: colors, spinners, progress rendering. |

Flags always win when both are set.

After a successful transfer, fsend checks GitHub at most once a day for a
newer release and, if one exists, prints a one-line hint to run
`fsend --update`. The check is skipped when stderr isn't a terminal (so
piped or scripted runs never trigger it) or `--quiet` is set, and can be
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
front of `:8080`. The file data on UDP/443 is already end-to-end
encrypted, but the HTTP pairing channel carries session slots and bearer
tokens in plaintext, so it has to run behind TLS. See
[Self-hosting](self-hosting.md) for the ready-made Caddy + Docker stack.

## Exit codes

Stable from v1.0.0. `0` means success; non-zero codes map to a specific
failure. For what to *do* about a given error, see
[Troubleshooting](troubleshooting.md).

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
| `13`  | `E013` — one or more files differed and were kept (transfer otherwise completed); use `--overwrite` to replace them. |
| `14`  | `E014` — could not reach the other side, even via the fallback relay. |
| `15`  | `E015` — the two sides couldn't agree how to transfer (protocol mismatch). |
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
| `31`  | `E031` — transfer requires a password the receiver didn't have (`--password` / `FSEND_PASSWORD`). |
| `32`  | `E032` — the other side cancelled the transfer. |
| `33`  | `E033` — `fsend --update` could not complete. |
| `34`  | `E034` — the other device is running an incompatible fsend version. |
| `35`  | `E035` — `fsend --uninstall` could not remove the binary. |
| `36`  | `E036` — a symlink in the send set can't be followed (missing or unreadable target, or a link cycle). |
| `37`  | `E037` — the server's relay spent its daily byte budget; forwarding is paused until 00:00 UTC. |
| `38`  | `E038` — files received, but the `--manifest` record could not be written. |
| `99`  | `E099` — unexpected error. Run with `--debug` and file an issue. |
| `130` | `E026` — cancelled by user (Ctrl-C / SIGTERM). |
| `141` | The output pipe closed early (e.g. `fsend <code> --out - \| head`). Shell convention (128+SIGPIPE); nothing is printed. |

`5` is unused. `16` isn't in this list because `E016` (corrupt config) is
a non-fatal warning that exits `0` — see the `0` row above.

## Share codes

Codes look like `abc-defg-jkm` — three groups (3-4-3) from a 23-letter
alphabet (`i`, `l`, `o` are excluded for legibility). One-shot,
system-generated, server-side TTL.

To require a password before the receiver can download, add `--password`
when sending. For the full model — entropy, TTL, rate-limiting, channel
binding — see [Security → Share codes](security.md#share-codes).
