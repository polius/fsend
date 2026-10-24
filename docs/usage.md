# Usage

Reference for the `fsend` CLI and its `server` subcommand. For inline
help, run `fsend --help` (the common flags), `fsend help advanced` (every
other flag, environment variables and exit status), or
`fsend server --help`.

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
| Use a server for one run | `fsend report.pdf --server fs.example.com` |
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
  ⇢  Sending report.pdf  ·  1 file  ·  2.4 MB

  On the other machine, run:

      fsend abc-defg-jkm

  The code works for one receiver, while this stays open.

⠋ Waiting for receiver  ·  3s  ·  Ctrl-C to cancel
```

Share that code (`abc-defg-jkm`) with the receiver any way you like —
chat, email, out loud. **On the receiver**, run it; fsend shows who's
sending and what, then asks before writing anything:

```console
$ fsend abc-defg-jkm
  ⇣  Incoming from mbp  ·  local network

      report.pdf  ·  1 file  ·  2.4 MB

  Save to ./? [Y/n] y
✓ Saved ./report.pdf — 2.4 MB in 188ms (12.7 MB/s)  ·  local network
```

Meanwhile the sender reports who accepted and finishes with a matching
line:

```console
✓ Receiver connected  ·  local network
✓ mbp accepted
✓ Sent report.pdf to mbp — 2.4 MB in 344ms (7 MB/s)  ·  local network
```

The file lands in the current directory (use `--out <dir>` to change
that, or `--yes` to skip the prompt). The trailing chip names the route:
`local network`, `direct`, or `relay`. That's the whole flow —
everything below is detail.

## Sending

```text
fsend [file|dir]... [flags]
```

Share the code (`abc-defg-jkm`) with the receiver. The code works for
one receiver, for as long as the sender keeps running (each pairing round
waits up to an hour). Piped stdin (a pipe or a non-empty redirected file)
is sent without a positional argument; an empty or `/dev/null` stdin shows
help instead. When stdout isn't a terminal, the bare code is also printed
there (`code=$(fsend f)`).

A file the sender can't read (permissions, or it vanished or shrank
mid-send) is skipped and listed on both sides; everything else still
transfers, and both sides exit `E050`. A file that changes while it's
being read is still sent, with a warning on both sides that the copy may
be inconsistent. One transfer can hold up to 4,000,000 files and
folders; past that the sender refuses before issuing a code (`E051`).

| Flag | Purpose |
|---|---|
| `--text <string>` | Send a literal string instead of a file. The receiver prints it to stdout; nothing is saved to disk. To keep it: `fsend <code> > note.txt`. |
| `--password[=<password>]` | Require the receiver to supply this password. Bare `--password` prompts interactively with a random four-word default; supply it inline as `--password=<password>`. Also `FSEND_PASSWORD`. |
| `--exclude <glob,…>` | Skip entries when bundling a directory. |
| `--as <name>` | File name the receiver saves a piped stream as (`pg_dump db \| fsend --as db.sql`). Default: `fsend-stdin-<random>`. |
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

By default the receiver sees the sender's device name and what they're
sending, then confirms. Pass `--yes` to skip. Codes are forgiving: any
case, spaces, underscores or dots for hyphens (`abc defg jkm`), no
hyphens at all (`abcdefgjkm`), surrounding quotes and a trailing full
stop all work — unless a file by that exact name exists, which is sent
instead. To keep the code out of the process list on a shared machine,
use `FSEND_CODE=abc-defg-jkm fsend`, or bare `fsend --receive` to be asked
for it.

Before the prompt, fsend checks there is enough free space for what it
is about to write; if not, it stops with `E008` before anything is
written.

| Flag | Purpose |
|---|---|
| `-y`, `--yes` | Auto-accept. Files that already exist and differ are kept (exit `E013`) unless you also pass `--overwrite` or `--rename`. |
| `-o`, `--out <dir>` | Receive into this directory (created if missing, like `mkdir -p`). Default: cwd. |
| `-o -`, `--out -` | Stream the payload to stdout instead of saving it. A single file, text or piped stream arrives as raw bytes; a folder or several files arrive as a plain (uncompressed) tar stream: `fsend <code> -o - \| tar x`. Retries are disabled: emitted bytes can't be rewound. |
| `--overwrite` | Replace existing files whose contents **differ**. A folder is never deleted to make room for a file. (Identical files are skipped either way.) |
| `--rename` | Keep both: save each differing file next to yours as `name (1).ext`. |
| `--manifest <file>` | After receiving, write a CSV record (`path,size,status,sha256`) to `<file>` — what fsend did with each file (`new` / `identical` / `overwritten` / `renamed` / `kept` / `resumed` / `skipped`) and the SHA-256 of the content written (empty when no bytes moved this run, e.g. identical or kept files). |
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
`FSEND_PASSWORD`) aborts the attempt **on the first try** — with no human
present to retry, failing fast beats hanging a script. The sender keeps
waiting with a warning, so the receiver can rerun the same command with
the right password **on the same device** (the code itself is already
spent; see [Resuming](#resuming-an-interrupted-transfer)). After three
such rounds the sender gives up and a fresh code is needed.

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

Byte-identical files are always skipped silently, and if everything is
already up to date there is no prompt at all. When some files *do*
differ, the accept prompt lists them (up to five; `l` lists all) and asks
once what to do:

```console
  1 file here already exists and differs:
    a.txt  − 2 B → + 8 B
  Save to ./?  [K]eep both · [r]eplace (deletes yours) · [s]kip them · [n]o
```

- **Keep both** (the default, also `y` or Enter) saves the incoming copy
  as `a (1).txt`.
- **Replace** overwrites your copy. Folders are never deleted to make
  room for a file.
- **Skip them** keeps your copies and receives everything else. That's
  your decision, so fsend exits 0.
- **No** declines the whole transfer (also exit 0).

Without a terminal to ask (`--yes`), differing files are kept and the
receiver exits `E013`, unless you pass `--overwrite` (replace) or
`--rename` (keep both). `--manifest <file>` records the exact per-file
outcome after a receive.

Two names that your filesystem treats as one — `Report.pdf` and
`report.pdf` on a case-insensitive disk, or the NFC and NFD spellings of
`café` on macOS — would overwrite each other, so fsend saves the first,
skips the other, lists it, and exits `E050`. fsend probes the receiving
folder's actual behaviour rather than guessing from the OS. The name
`.fsend` is reserved for partial downloads (see below).

### Verifying what you received

fsend verifies every file against a BLAKE3 root sent by the sender
before it keeps it. For an independent check, `--manifest <file>` records
the SHA-256 of each file written this run; compare it against
`shasum -a 256 <file>` on the sender. (The terminal doesn't print hashes.)

The progress bar carries a route chip — `local network`, `direct`, or
`relay` — showing which path the bytes are taking. A successful transfer
that took 10 seconds or more rings the terminal bell and raises a desktop
notification (when stderr is a terminal and `--quiet` is not set;
`FSEND_NO_NOTIFY=1` turns it off).

### Resuming an interrupted transfer

If a transfer is interrupted, fsend keeps what it already received in a
hidden `.fsend/` folder inside the receive directory and continues from
there next time — only the missing bytes transfer. Short drops (a Wi-Fi
blip, a network change) are retried automatically without you doing
anything.

While the sender's fsend is **still running**, rerun the same
`fsend <code>` **on the same device** — the sender is back to waiting,
and the transfer resumes. This works because after the first successful
handshake both sides switch from the code to a resume key only they
hold; the receiver saves it in its config directory for up to 24 hours.
The code itself is spent: anyone else who tries it gets `E003` for the
next ten minutes (then `E002`), so nobody can step into your dropped
transfer.

Once the sender has exited, resuming is a fresh send with a new code:

1. **Sender** runs the original command again. fsend issues a *new* code.
2. **Share the new code** with the receiver (the old one no longer works).
3. **Receiver** runs the new code **in the same directory** as before.
   fsend finds the partial in `.fsend/` and picks up where it stopped.

Before resuming, the sender checks the receiver's whole partial against
its own file. If the source changed or the prefix doesn't match, that
file is sent again from the start — you never resume onto stale data.
Partials untouched for seven days are removed by the next receive into
that folder, and the `.fsend/` folder is removed once it is empty.

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

The pipe carries one raw stream — no names — so redirect to the name you
want on C. A folder sent from A leaves B as a tar stream (that's what
`--out -` does with folders), so on C unpack it with
`fsend <codeB> --out - | tar x`. Because B decrypts each hop to
re-send it, only chain through a machine you trust.

## Choosing a server (`--connect`)

The default pairing server is `fsend.alzina.dev` — best-effort, free,
not guaranteed. Switch any time:

```sh
fsend --connect fs.example.com:443           # persisted
fsend --connect fs.example.com:443,mySecret  # password-gated server
fsend --connect default                      # back to the public default
fsend --connect                              # show current
fsend --connect http://10.0.0.5:8080         # plain HTTP (LAN-only server)
fsend report.pdf --server fs.example.com     # this run only, not saved
```

Without a scheme the server is reached over HTTPS (port 443 by default;
plain HTTP for `localhost`). `--server` and `FSEND_SERVER` take the same
forms but never touch the config file — handy in CI.

Config is at `~/.config/fsend/config.json` (Linux),
`~/Library/Application Support/fsend/config.json` (macOS), or
`%LOCALAPPDATA%\fsend\config.json` (Windows). See
[Self-hosting](self-hosting.md) to run your own.

## Shared flags

| Flag | Purpose |
|---|---|
| `--send` / `--receive` | Force mode instead of auto-detecting from the argument. Mutually exclusive; handy in scripts. |
| `--device-name <string>` | Override the device name shown to the peer (default: hostname) — the sender's name in the receiver's `Incoming from …` line, the receiver's name in the sender's `… accepted` and summary lines. (`--name` still works.) |
| `--server <host[:port]>` | Use this pairing server for this run only; the config file is untouched. Also `FSEND_SERVER`. |
| `-q`, `--quiet` | Suppress non-error output. On send, prints **just the share code** to stdout (see [Scripting](#scripting)); on receive, requires `--yes`. |
| `--debug` | Verbose logging to stderr. Also `FSEND_DEBUG=1`. |
| `--update` | Update fsend to the latest release by re-running the installer in place. Reports when already up to date. |
| `--update --check` | Report whether a newer release exists — printing the same hint as the once-a-day notice, with the release-notes link — and exit without installing. Exits 0 either way. |
| `--uninstall` | Remove the binary and **recursively delete** the fsend config directory (see [`--connect`](#choosing-a-server---connect) for the per-OS path). `--yes` skips confirmation. |
| `--help` / `-h` | Show inline help. `fsend help advanced` shows the rest. |
| `--version` / `-v` | Print version. |

## Environment variables

| Variable | Equivalent flag | Purpose |
|---|---|---|
| `FSEND_PASSWORD` | `--password` | Supply the transfer password out-of-band. |
| `FSEND_PASSWORD_FILE` | `--password` | Read the transfer password from this file (trailing newline trimmed). Used only when `FSEND_PASSWORD` is unset; keeps the secret out of both argv and the environment. |
| `FSEND_SERVER_PASSWORD` | `--connect <host:port>,<password>` | Shared secret for a self-hosted pairing server. |
| `FSEND_SERVER_PASSWORD_FILE` | `--connect <host:port>,<password>` | Read the server password from this file instead. Used only when `FSEND_SERVER_PASSWORD` is unset. |
| `FSEND_CODE` | `fsend <code>` | The code to receive, kept out of `ps` on shared machines. Used when no path or code is given (`FSEND_CODE=… fsend`), and by bare `--receive`. |
| `FSEND_SERVER` | `--server` | Pairing server for this run, without saving it. |
| `FSEND_THEME` | — | `light` or `dark`: tune colors to your terminal background. Unset, fsend uses colors that read on both. |
| `FSEND_PLAIN` | — | Set to `1` for plain output — no colors, animation or Unicode symbols, one line per update — for screen readers and logs. Automatic when the locale isn't UTF-8. |
| `FSEND_NO_NOTIFY` | — | Set to `1` to silence the bell and desktop notification fsend sends when a transfer that took 10 s or more succeeds. |
| `FSEND_DEBUG` | `--debug` | Set to `1` (any value except `0`/`false`) for verbose stderr logging. |
| `FSEND_NO_UPDATE_CHECK` | — | Set to `1` (any value except `0`/`false`) to disable the once-a-day check for a newer release. |
| `NO_COLOR` | — | Set to any non-empty value to disable ANSI colors ([no-color.org](https://no-color.org)). |
| `FORCE_COLOR` | — | Set to `1` (any value except `0`/`false`) to force ANSI colors on, even when output isn't a terminal. Colors only — spinners and progress bars still require a terminal. |
| `TERM` | — | `TERM=dumb` disables all ANSI escapes: colors, spinners, progress rendering. |

Flags always win when both are set.

While a transfer runs, fsend checks GitHub in the background at most once a
day for a newer release (never delaying the exit) and, if one exists, prints
a one-line hint after the summary with the release
link so you can see what changed — and the command that actually works for
this install (`brew upgrade fsend` on a Homebrew-managed binary, instead of
an `fsend --update` that would only refuse with [E039](#exit-codes)). The
check is skipped when stderr isn't a terminal (so piped or scripted runs
never trigger it) or `--quiet` is set, and can be turned off entirely with
`FSEND_NO_UPDATE_CHECK=1`.

## `fsend doctor`

A read-only diagnostic covering what a transfer depends on. Each
warning says what it affects and what still works. Exits `0` when every
check passes and `43` (`E043`) when any is degraded, so scripts can gate
on it. It checks the saved server, or `FSEND_SERVER` when set:

```text
$ fsend doctor
fsend doctor — checking what a transfer depends on

✓ config   server fsend.alzina.dev:443 (default)
✓ config   FSEND_SERVER overrides it: fs.example.com:443
✓ server   reachable in 1ms
✓ UDP      relay fs.example.com:443 answered in 1ms
✓ TCP      relay fallback over TCP works (echo in 1ms)
✓ network  LAN address 192.168.1.85
✓ mDNS     local roundtrip ok in 3ms
✓ mDNS     answered on the LAN in 30ms

✓ All 8 checks passed. Transfers will try your local network first.
```

A degraded run, against a server that predates the TCP fallback (shown
as it prints when piped or with `FSEND_PLAIN=1`, where the glyphs become
`[OK]`, `[!]` and `[FAIL]`):

```text
fsend doctor — checking what a transfer depends on

[OK] config   server fsend.alzina.dev:443 (default)
[OK] server   reachable in 152ms
[OK] UDP      relay fsend.alzina.dev:443 answered in 54ms
[!] TCP      this server has no TCP fallback for its relay — on a network that blocks UDP, cross-network transfers will fail
[OK] network  LAN address 192.168.1.85
[OK] mDNS     local roundtrip ok in 13ms
[OK] mDNS     answered on the LAN in 34ms

[FAIL] [E043] Some checks found a problem — see the warnings above.
  Each one says what it affects and what still works.
```

| Check | Meaning |
|---|---|
| `config` | Which pairing server is configured (custom vs default; the server password is never printed). |
| `server` | HTTPS reachability of that server's `/health`. Unreachable → cross-network transfers fail, LAN transfers are unaffected. |
| `UDP` | The server's relay answers a STUN request over UDP. No reply → the network blocks UDP, and relayed transfers need the TCP fallback. |
| `TCP` | The relay's TCP fallback (a WebSocket on the server's HTTPS port) echoes a message. Used automatically when UDP is blocked. |
| `network` | The local LAN address fsend would announce. VPN/container interfaces are skipped. |
| `mDNS` (roundtrip) | fsend's mDNS machinery tested over loopback — deterministic, the OS cannot filter it. A miss here means LAN discovery is unavailable entirely. |
| `mDNS` (LAN answer) | Multicast across the physical interface. Blocked → discovering *other* devices falls back to the pairing server (transfers still complete; same-device transfers keep working). |

## `fsend server`

Same binary, run with the `server` subcommand. Configured entirely via
environment variables.

```text
fsend server                 Run the server
fsend server --health-check  Probe /health (exit 0 if healthy)
fsend server --help          Show help
```

### Server environment variables

Full table with defaults, units, and notes:
[Self-hosting → Configuration](self-hosting.md#configuration).

Quick LAN-only run (no TLS — local testing only):

```sh
docker run -p 443:443/udp -p 127.0.0.1:8080:8080/tcp poliuscorp/fsend server
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
| `0`   | Success (or non-fatal warning, e.g. `E016` config corrupted) — or your own "no" at a prompt: declining a transfer (`E006` on the receiver), choosing "skip them" at the existing-files prompt (`E013`), answering `n` to `--uninstall`. The line still names the code; only the exit is 0. |
| `1`   | `E001` — server unreachable. |
| `2`   | `E002` — code not found. |
| `3`   | `E003` — code already claimed by another receiver. |
| `4`   | `E004` — invalid code format. |
| `6`   | `E006` — the receiver declined the transfer (sender side). |
| `7`   | `E007` — no receiver paired within the hour a pairing round lasts (sender side). |
| `8`   | `E008` — disk full. |
| `9`   | `E009` — could not write the target file. |
| `10`  | `E010` — could not read the source file. |
| `11`  | `E011` — file arrived corrupted; hash didn't match. |
| `12`  | `E012` — path traversal rejected. |
| `13`  | `E013` — one or more files differed and were kept without being asked (`--yes`, no terminal; transfer otherwise completed); use `--overwrite` to replace them or `--rename` to keep both. |
| `14`  | `E014` — could not reach the other side, even via the fallback relay. |
| `15`  | `E015` — the two sides couldn't agree how to transfer (protocol mismatch). |
| `17`  | `E017` — rate limited. |
| `18`  | `E018` — default server retired. |
| `19`  | `E019` — a partial download didn't match its source. Current releases resend that file from the start instead, so you shouldn't see this. |
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
| `39`  | `E039` — `fsend --update`/`--uninstall` on a Homebrew-managed binary; use `brew upgrade`/`brew uninstall fsend`. |
| `40`  | `E040` — `fsend --update` as root; opt in with `FSEND_ALLOW_ROOT=1` on single-user machines (containers). |
| `41`  | `E041` — `fsend --update` from a development build; there is no release to compare against. Install a release build. |
| `42`  | `E042` — the network blocks UDP and the relay's TCP fallback failed too (or the server doesn't offer it). |
| `43`  | `E043` — `fsend doctor` found at least one degraded check. |
| `46`  | `E046` — the sender stopped after 10 failed handshakes on its code (someone may be guessing it). |
| `47`  | `E047` — the pairing server's TLS certificate isn't trusted (captive portal or intercepting proxy). |
| `50`  | `E050` — the transfer finished, but some files were skipped (unreadable on the sender, or a name that can't be placed on the receiver). |
| `51`  | `E051` — more than 4,000,000 files and folders in one transfer. |
| `60`  | `E060` — the pairing server refused this fsend as too old (HTTP 426). Run `fsend --update`. |
| `61`  | `E061` — `fsend server` stopped because its relay UDP socket failed; the restart policy should bring it back. |
| `99`  | `E099` — unexpected error. Run with `--debug` and file an issue. |
| `130` | `E026` — cancelled by user (Ctrl-C / SIGTERM). |
| `141` | The output pipe closed early (e.g. `fsend <code> --out - \| head`). Shell convention (128+SIGPIPE); nothing is printed. |

`5` is unused. `16` isn't in this list because `E016` (corrupt config) is
a non-fatal warning that exits `0` — see the `0` row above.

## Share codes

Codes look like `abc-defg-jkm` — three groups (3-4-3) from a 23-letter
alphabet (`i`, `l`, `o` are excluded for legibility). They're
system-generated and work for exactly one receiver: once a receiver has
connected, the code is spent and only that device can resume. Each
pairing round lasts up to an hour, and the code dies when the sender
exits.

To require a password before the receiver can download, add `--password`
when sending. For the full model — entropy, TTL, rate-limiting, channel
binding — see [Security → Share codes](security.md#share-codes).
