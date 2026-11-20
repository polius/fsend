# Usage

`fsend --help` shows the common flags. `fsend help advanced` shows the
rest, the environment variables and the exit codes.

## Quick reference

| Goal | Command |
|---|---|
| Send a file | `fsend report.pdf` |
| Send a folder | `fsend ./project` |
| Send several files | `fsend a.txt b.txt c.txt` |
| Send from a pipe | `cat file.bin \| fsend` |
| Send text | `fsend --text "hello world"` |
| Receive | `fsend abc-defg-jkm` |
| Receive without a prompt | `fsend --yes abc-defg-jkm` |
| Use your own server | `fsend --connect fs.example.com:443` |
| Back to the public server | `fsend --connect default` |

fsend picks send or receive from the argument: a code means receive,
anything else means send. If an argument is both a code and a file, it
asks. The same goes for a file or folder named like a command (`server`,
`doctor`, `help`, `completion`): in a terminal it asks, in a script the
command runs and `fsend --send server` sends the folder. In scripts, use
`--send` or `--receive`.

## Your first transfer

**On the sender:**

```console
$ fsend report.pdf

→ Sending report.pdf
  1 file · 2.4 MB

  On the other machine, run

      fsend abc-defg-jkm

  One receiver · valid while this runs

  No fsend there yet? This installs it and receives:
    curl -fsSL https://getfsend.alzina.dev | sh -s -- abc-defg-jkm
    iex "& {$(irm https://getfsend.alzina.dev/windows)} -Code abc-defg-jkm"

⠋ Waiting for receiver · 4 s · Ctrl-C to cancel
```

Share the code any way you like. The two install lines are for a
receiver without fsend: the first for macOS and Linux, the second for
Windows PowerShell.

**On the receiver:**

```console
~/Downloads $ fsend abc-defg-jkm

← Incoming report.pdf from macbook
  1 file · 2.4 MB · local network

? Save to ~/Downloads/  [Y/n] y
✓ Saved ~/Downloads/report.pdf
  2.4 MB · 8 ms · local network
```

The last line names the route: `local network`, `direct` or
`relay via <server>`.

## Sending

```text
fsend [file|dir]... [flags]
```

- The code works for **one receiver**, while the sender keeps running.
- Piped input is sent without an argument: `tar c ./build | fsend`.
- When stdout isn't a terminal, the code is also printed there:
  `code=$(fsend file)`.
- The sender copies `fsend <code>` to the clipboard (never with
  `--password`).

| Flag | What it does |
|---|---|
| `--text <string>` | Sends text. The receiver prints it instead of saving a file. |
| `--password[=<pw>]` | Requires a password. Bare `--password` prompts, with a random default. Also `FSEND_PASSWORD`. |
| `--exclude <glob,…>` | Skips matching entries in a folder: `--exclude 'node_modules,*.log'`. |
| `--as <name>` | Name for a piped stream: `pg_dump db \| fsend --as db.sql`. |
| `--preview` | Lists what would be sent (CSV: `path,size`) and exits. |

**Files that can't be sent:**

- A file in a folder that can't be read, or that vanishes mid-send, is
  skipped. Both sides list it and exit `E050`.
- A file named on the command line that can't be read stops the send
  with `E010`.
- A file that changes while being read is still sent, with a warning.
- One transfer holds up to 4,000,000 files and folders (`E051` above).

**Symlinks** are followed: the receiver gets a real copy of the target.
A broken link inside a folder is skipped (`E050`). A broken link named on
the command line, or a link loop, stops the send with `E036`.

## Receiving

```text
fsend <code> [flags]
```

The receiver sees who is sending what, then confirms. Codes are
forgiving: any case, spaces or dots instead of hyphens, or no hyphens at
all. `fsend abc defg jkm` works too.

To keep the code out of the process list, use
`FSEND_CODE=abc-defg-jkm fsend`, or `fsend --receive` to be asked for it.

| Flag | What it does |
|---|---|
| `-y`, `--yes` | Accepts without asking. Existing files that differ are kept (`E013`), unless you add `--overwrite` or `--rename`. |
| `-o`, `--out <dir>` | Saves into `<dir>`, created if needed. Default: the current folder. |
| `-o -` | Writes to stdout. A folder arrives as a tar stream: `fsend <code> -o - \| tar x`. |
| `--overwrite` | Replaces existing files that differ. |
| `--rename` | Keeps both, saving the new one as `name (1).ext`. |
| `--checksum` | Compares file contents instead of size and date. Slower, but catches every change. |
| `--manifest <file>` | Writes a CSV of what happened to each file, with its SHA-256. |
| `--password=<pw>` | The sender's password. Also `FSEND_PASSWORD`. |

fsend checks for free space before writing (`E008` if there isn't
enough).

**Passwords.** Typed at the prompt, you get 3 tries. Given with
`--password=` or `FSEND_PASSWORD`, a wrong one fails at once. The sender
keeps waiting, so you can retry on the same device.

### When a file already exists

Re-sending a folder moves only what's new or changed. By default a file
with the same name, size and date counts as identical. `--checksum`
compares contents instead.

When files differ, fsend asks once:

```console
? 1 file here already exists and differs
    a.txt   2 B → 8 B   yours is newer by 3 h
! Yours is newer — replace would lose your changes
? Save to ~/Downloads/  [K]eep both · [r]eplace (deletes yours) · [s]kip them · [n]o
```

- **Keep both** (default): saves the new copy as `a (1).txt`.
- **Replace**: overwrites yours. A non-empty folder is never deleted.
- **Skip them**: keeps yours and receives the rest.
- **No**: declines the transfer.

**Names that can't be saved** are skipped, listed on both sides, and
both exit `E050`:

- Two names your disk treats as one (`Report.pdf` and `report.pdf` on a
  case-insensitive disk). The first is saved.
- A name the system refuses, like `aux.c` on Windows or one longer than
  255 bytes.

The name `.fsend` is reserved for partial downloads.

### Verifying what you received

fsend checks every file against a hash sent by the sender before keeping
it. For your own check, `--manifest` records each file's SHA-256 to
compare with `shasum -a 256` on the sender.

### Resuming an interrupted transfer

Partial data is kept in a hidden `.fsend/` folder. Only the missing bytes
move next time, and short network drops are retried on their own.

- **Sender still running:** run the same `fsend <code>` again on the
  same device. The code works only for that device now; anyone else gets
  `E003`.
- **Sender has exited:** the sender runs its command again and gets a
  new code. The receiver runs the new code **in the same folder**.

If the source file changed, it is sent again from the start. Partials
untouched for 7 days are deleted.

## Scripting

With `--quiet`, the sender prints only the code on stdout. Everything
else goes to stderr. The receiver needs `--yes` with it.

```sh
fsend file.txt --quiet > code.txt
FSEND_PASSWORD=swordfish fsend "$(cat code.txt)" --quiet --yes --out ~/incoming
```

### JSON output (`--json`)

`--json` prints one JSON object per line on stdout. Human output stays on
stderr, and exit codes don't change.

```json
{"v":1,"event":"code","code":"abc-defg-jkm"}
{"v":1,"event":"peer","name":"mbp","route":"local"}
{"v":1,"event":"accepted"}
{"v":1,"event":"progress","bytes":1048576,"total":2100000}
{"v":1,"event":"done","ok":true,"role":"sender","exit":0,"bytes_total":2100000,"bytes_moved":2100000,"files_sent":1,"files_skipped":0,"files_kept":0,"duration_ms":312,"route":"local"}
```

| Event | When | Fields |
|---|---|---|
| `code` | Sender: the code is ready | `code`, and `install` / `install_powershell` (the install-and-receive lines; only with the default server) |
| `peer` | The other side connected | `name`, `route` |
| `accepted` | The receiver said yes | — |
| `retry` | A dropped connection is retried | `attempt`, `of`, `wait_ms` |
| `progress` | While data moves, at most once a second | `bytes`, `total` (not for a pipe), `file` |
| `done` | At the end | see below |

`done` fields:

| Field | Meaning |
|---|---|
| `ok`, `error`, `exit` | The outcome. `error` is the code, such as `"E013"`. |
| `role` | `sender` or `receiver`. Missing if the run failed before either started. |
| `bytes_total` | Sender: the size offered. Receiver: the bytes it accepted. |
| `bytes_moved` | Bytes that crossed the network in this run. |
| `files_sent`, `files_skipped`, `files_kept` | Sender counts. |
| `files_saved`, `files_up_to_date`, `files_kept` | Receiver counts. |
| `duration_ms`, `route` | Time from the first byte; `local`, `direct` or `relay`. |
| `dir` | Receiver: the folder files were saved in. |
| `text` | Receiver: the text of a `--text` send. |

An early failure carries only `ok`, `error` and `exit`. New fields may
be added; `v` changes only on a breaking change. `--json` can't be
combined with `--preview` or `--out -`.

## Chaining through a middle machine

If A and C can't reach each other but B reaches both, chain two
transfers on B. B never writes the data to disk, but it does decrypt it,
so only use a machine you trust.

```sh
fsend file.bin                          # A: prints code A
fsend <codeA> --out - --yes | fsend     # B: prints code B
fsend <codeB> --out - > file.bin        # C: saves it
```

A folder leaves B as a tar stream: on C, use
`fsend <codeB> --out - | tar x`.

## Choosing a server (`--connect`)

The default server is `fsend.alzina.dev`: free, best effort.

```sh
fsend --connect fs.example.com:443           # save a server
fsend --connect fs.example.com:443,mySecret  # with a password
fsend --connect default                      # back to the public server
fsend --connect                              # show the current one
fsend report.pdf --server fs.example.com     # this run only
```

Without a scheme, fsend uses HTTPS. `--server` and `FSEND_SERVER` never
change the config file, and don't use its saved password (set
`FSEND_SERVER_PASSWORD`).

The config file is at:

- Linux: `~/.config/fsend/config.json`
- macOS: `~/Library/Application Support/fsend/config.json`
- Windows: `%LOCALAPPDATA%\fsend\config.json`

To run your own server, see [Self-hosting](self-hosting.md).

## Other flags

| Flag | What it does |
|---|---|
| `--send`, `--receive` | Forces send or receive. |
| `--device-name <name>` | The name the other side sees. Default: the hostname. |
| `--server <host>` | Uses this server for one run. Also `FSEND_SERVER`. |
| `--local` | Local network only. Never contacts the server. |
| `-q`, `--quiet` | Errors only. The sender still prints the code. |
| `--debug` | Detailed logs on stderr. Also `FSEND_DEBUG=1`. |
| `--update` | Updates fsend. `--update --check` only reports. |
| `--uninstall` | Removes fsend and its config folder. `--yes` skips the question. |
| `-h`, `--help` / `-v`, `--version` | Help and version. |

## Environment variables

Flags win over variables.

| Variable | What it does |
|---|---|
| `FSEND_PASSWORD` | The transfer password. |
| `FSEND_PASSWORD_FILE` | Reads the transfer password from a file. |
| `FSEND_SERVER_PASSWORD` | Password for your own server. Never sent to the public one. |
| `FSEND_SERVER_PASSWORD_FILE` | Reads that password from a file. |
| `FSEND_CODE` | The code to receive, kept out of the process list. |
| `FSEND_SERVER` | The server for this run. |
| `FSEND_THEME` | `light` or `dark`, to match your terminal. |
| `FSEND_PLAIN=1` | Plain output: no colors, animation or symbols. For screen readers and logs. |
| `FSEND_NO_NOTIFY=1` | No bell or desktop notification after a run of 10 s or more. |
| `FSEND_NO_CLIPBOARD=1` | Doesn't copy the receive command to the clipboard. |
| `FSEND_NO_UPDATE_CHECK=1` | Doesn't check for new releases. |
| `FSEND_DEBUG=1` | Same as `--debug`. |
| `NO_COLOR`, `FORCE_COLOR`, `TERM=dumb` | The usual color switches. |

Once a day, fsend checks for a new release in the background. If there
is one, it prints a short notice with the right update command for your
install. It never checks when output isn't a terminal or with `--quiet`.

## `fsend doctor`

Checks everything a transfer depends on. Each problem says what it
affects and how to fix it. Exits `0` when all checks pass, `43` (`E043`)
otherwise.

```text
$ fsend doctor
fsend doctor — checking what a transfer depends on

✓ config   server fsend.alzina.dev:443 (default)
✓ server   reachable in 152 ms
✓ clock    matches the server's
✓ UDP      relay fsend.alzina.dev:443 answered in 54 ms
✓ TCP      relay fallback over TCP works (echo in 61 ms)
✓ NAT      behind NAT, public address 203.0.113.7 — direct paths need hole
           punching; when it fails, transfers use the relay
✓ network  LAN address 192.168.1.85
✓ IPv6     no global address (IPv4 only) — fine; direct paths use IPv4
✓ listener answered at 192.168.1.85:50412 over TCP in 16 ms and QUIC in 209 ms
           dialled from this machine: the connection never left it, so this
           shows the listener works, not that other devices get past a firewall
✓ firewall macOS firewall allows incoming connections to fsend
✓ mDNS     local roundtrip ok in 3 ms
✓ mDNS     answered on the LAN in 30 ms

✓ All 12 checks passed. Transfers will try your local network first.
```

`✓` passed, `!` is a problem. A row with no mark is something doctor
couldn't find out, such as firewall rules that need root. It doesn't
count as a failure.

| Check | What it tests |
|---|---|
| `config` | Which server is set. |
| `server` | The server answers. If not, transfers between networks fail; local ones still work. |
| `clock` | Your clock against the server's. A wrong clock breaks local discovery. |
| `UDP` | The relay answers over UDP. If not, the relay falls back to TCP. |
| `TCP` | The relay's TCP fallback works, for networks that block UDP. |
| `NAT` | Whether you're behind NAT. Information only. |
| `network` | The local address fsend announces. |
| `IPv6` | Whether you have a public IPv6 address. Information only. |
| `listener` | fsend's local listener answers. See [below](#the-listener-check). |
| `firewall` | Your firewall lets other devices reach it. See [below](#the-firewall-check). |
| `mDNS` | Local discovery works on this machine and on the network. |

### The listener check

When you send, fsend listens on one port between 50000 and 50999 (picked
from the code), over TCP and UDP. This check opens that listener and
connects to it from this machine, like a receiver would.

A pass shows the listener works. It can't show that **another** device
gets through your Wi-Fi or firewall: the connection never leaves this
machine. On macOS it does show the firewall allows fsend. Only a real
transfer from a second device proves the rest.

### The firewall check

Reads your firewall's rules and says whether other devices can reach
fsend's ports (TCP and UDP 50000–50999):

- **macOS** and **Windows:** fully, without admin rights.
- **Linux (ufw, firewalld):** what an ordinary user can read. Otherwise
  it says the rules are unread.
- **Linux (nftables, iptables):** needs `sudo fsend doctor`.

If the firewall blocks fsend, local transfers still work through the
server, just slower. To open the ports:

```sh
# ufw
sudo ufw allow 50000:50999/udp && sudo ufw allow 50000:50999/tcp

# firewalld (use the zone doctor names)
sudo firewall-cmd --zone=public --add-port=50000-50999/udp --add-port=50000-50999/tcp
sudo firewall-cmd --runtime-to-permanent
```

On macOS: System Settings → Network → Firewall → Options, allow fsend.
On Windows: choose **Allow** when Windows asks, or run as administrator:

```bat
netsh advfirewall firewall add rule name="fsend" dir=in action=allow program="C:\path\to\fsend.exe" enable=yes
```

This exposes nothing to the internet: the listener accepts only local
devices, and each one must know the code.

## `fsend server`

The same binary runs the pairing server. It's configured with
environment variables; see [Self-hosting](self-hosting.md).

```text
fsend server                 Runs the server
fsend server --health-check  Exits 0 if the server is healthy
```

## Exit codes

`0` is success. It's also used for your own "no" at a prompt, such as
declining a transfer. A prompt that gets no input at all (cron, CI,
`ssh -T`) is not a "no": it exits `24`. For what to do about an error, see
[Troubleshooting](troubleshooting.md).

| Code | Meaning |
|---|---|
| `1` | `E001` Server unreachable. |
| `2` | `E002` Code not found. |
| `3` | `E003` Code already used by another receiver. |
| `4` | `E004` Invalid code. |
| `6` | `E006` The receiver declined (sender side). |
| `7` | `E007` No receiver within an hour. |
| `8` | `E008` Disk full. |
| `9` | `E009` Couldn't write a file. |
| `10` | `E010` Couldn't read a file. |
| `11` | `E011` A file arrived corrupted. |
| `12` | `E012` Unsafe path rejected. |
| `13` | `E013` Differing files were kept (`--yes`). Use `--overwrite` or `--rename`. |
| `14` | `E014` Couldn't reach the other side, even through the relay. |
| `15` | `E015` The two sides couldn't agree on how to transfer. |
| `17` | `E017` Rate limited. |
| `18` | `E018` The default server was retired. |
| `19` | `E019` A partial download didn't match its source. |
| `20` | `E020` Transfer failed after retries. |
| `21` | `E021` Wrong transfer password. |
| `22` | `E022` Couldn't verify the other side. |
| `23` | `E023` The server's size limit was reached. |
| `24` | `E024` Invalid usage. Also a receive with no input to answer the accept prompt and no `--yes`; the sender keeps waiting. |
| `25` | `E025` File or folder not found. |
| `27` | `E027` Couldn't open the local-network port. |
| `28` | `E028` The server needs a password. |
| `29` | `E029` The server closed an idle connection. |
| `30` | `E030` `fsend server` couldn't start. |
| `31` | `E031` The transfer needs a password. |
| `32` | `E032` The other side cancelled. |
| `33` | `E033` `--update` failed. |
| `34` | `E034` The other device runs an incompatible version. |
| `35` | `E035` `--uninstall` couldn't remove the binary. |
| `36` | `E036` A symlink can't be followed. |
| `37` | `E037` The relay used its daily budget. |
| `38` | `E038` The `--manifest` file couldn't be written. |
| `39` | `E039` Installed by a package manager; use it to update or remove. |
| `40` | `E040` `--update` as root. Set `FSEND_ALLOW_ROOT=1` to allow it. |
| `41` | `E041` `--update` from a development build. |
| `42` | `E042` UDP is blocked and the TCP fallback failed. |
| `43` | `E043` `fsend doctor` found a problem. |
| `46` | `E046` No longer used. |
| `47` | `E047` The server's certificate isn't trusted (captive portal or proxy). |
| `50` | `E050` Done, but some files were skipped. |
| `51` | `E051` More than 4,000,000 files and folders. |
| `60` | `E060` This fsend is too old for the server. |
| `61` | `E061` `fsend server` stopped: its relay socket failed. |
| `99` | `E099` Unexpected error. Run with `--debug` and open an issue. |
| `130` | `E026` Cancelled (Ctrl-C). |
| `141` | The output pipe closed early (e.g. `\| head`). |

## Share codes

Codes look like `abc-defg-jkm`: three groups of letters, without `i`,
`l` or `o`. A code works for one receiver and dies when the sender
exits. For how codes are protected, see
[Security → Share codes](security.md#share-codes).
