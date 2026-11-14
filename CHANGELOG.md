# Changelog

Every release's notes, newest first. The body of each
[GitHub release](https://github.com/polius/fsend/releases) is this file's
section for that version. The sections before 1.17 list only the changes
that needed action; the full notes of those releases are on GitHub. See
[Version compatibility](docs/compatibility.md) for which versions work
together.

## Unreleased

The wire protocol is unchanged (protocol 3). This release transfers with
1.16 in both directions and works with a 1.16 server.

### Upgrade notes

- **Self-hosted behind the bundled Caddy: redeploy with the new compose
  file.** It sets `FSEND_SERVER_TRUSTED_PROXIES`, so each client gets its
  own per-IP limits. Without it, every client behind Caddy shares one
  allowance, as before. Download `deploy/compose/docker-compose.yml`
  again (or add that line to yours), then run
  `docker compose pull && docker compose up -d`. Details under Added.
- **Firewalls: the sender now also listens for TCP on the local
  network**, on the port it already used for UDP (one port per code,
  50000 to 50999). Allow incoming TCP there to get the faster route. If
  TCP is blocked, the receiver falls back to QUIC over UDP after 300 ms,
  as before.
- **`FSEND_STATE_FILE` is gone.** Drop the variable and the `fsend_state`
  volume from your compose file. Details under Removed.

### Added

- **Linux packages.** Each release attaches `.deb`, `.rpm` and `.apk`
  files for x86-64, arm64, ARMv6, ARMv7, 32-bit x86 and riscv64. They
  install the binary to `/usr/bin`, the man pages, and completions for
  bash, zsh and fish. On a packaged install, `fsend --update` and
  `fsend --uninstall` stop with `E039` and name the package manager
  command to run instead.
- **Man pages and completions in the release archives**, in `manpages/`
  and `completions/`.
- **Install and receive in one line.** The installers take a share code
  and run the fresh fsend on it:
  `curl -fsSL https://getfsend.alzina.dev | sh -s -- <code>`, or in
  PowerShell
  `& ([scriptblock]::Create((irm https://getfsend.alzina.dev/windows))) -Code <code>`.
  The sender prints the first form under the share command, for a
  receiver who doesn't have fsend yet (only when it uses the default
  server).
- **The receive command goes to the clipboard.** The sender copies
  `fsend <code>` with `pbcopy`, `clip.exe`, `wl-copy`, `xclip` or
  `xsel`, and says so only when the copy worked. Over SSH it asks the
  terminal instead (OSC 52). Never for `--password` sends.
  `FSEND_NO_CLIPBOARD=1` turns it off.
- **`--local`**: use the local network only and never contact the
  pairing server, on either side. A receiver that finds no sender on the
  network gets `E002`. (It replaces the hidden `--mode=local`, which
  still works.)
- **More `--json` events.** Besides `code` and `done`, both sides now
  emit `peer` (the other device's name and the route), `accepted`,
  `retry` and `progress` (at most once a second). The `code` event
  carries `install`, the install-and-receive line. All additive to
  schema `v` 1; see [Usage](docs/usage.md#json-output---json).
- **`FSEND_SERVER_TRUSTED_PROXIES` (server).** A list of reverse proxies
  (IPs or CIDR ranges) whose `X-Forwarded-For` the server believes, so
  the per-IP limits apply to the client behind the proxy instead of to
  the proxy. Unset, forwarded headers are ignored, as before. The
  shipped compose file trusts the private ranges, where only Caddy
  reaches the server. A waiting sender's own polls no longer count
  against its address's rate limit.
- **Conflict prompt: which copy is newer.** Each differing file says
  "yours is newer by 3 h", "theirs is newer by 2 d" or "same age", and a
  warning appears when replacing would lose newer local changes.
- **`fsend doctor`** checks more: the device's clock against the
  server's, whether the machine is behind NAT, IPv6, and on macOS
  whether the firewall lets other devices connect to fsend.
- **A code typed as three words** works: `fsend abc defg jkm`.
- **Installers.** `install.ps1` reads `FSEND_NO_MODIFY_PATH=1`, and both
  installers print the one line that puts fsend on `PATH` in the current
  shell.

### Faster

Measured with both ends on one machine, the receiver writing to
`/dev/null`. Real transfers are also limited by the link and the disks.

- **Local network: a TCP route.** The receiver now connects over TCP
  first, with the same TLS 1.3, post-quantum key exchange and code check
  as before, and uses QUIC only when TCP doesn't connect. 1 GiB of
  random data: 72 → about 1,150 MB/s on macOS, about 500 → 1,430 MB/s on
  Linux. An older peer on either side keeps using QUIC.
- **Compression on several cores.** 512 MiB of source code over the
  local network: 160 → about 690 MB/s on macOS.
- **Direct route between networks**: 3–9 → 52–63 MB/s on macOS, and
  steady.
- **Relay**: 9–21 → 26–29 MB/s on macOS. (On Linux the direct route and
  the relay both move about 170 MB/s.) When the network blocks UDP, both
  sides reach the relay in about 2 s instead of 5 to 7.5 s.
- **Many small files.** 20,000 files of 1 KB: about 11 s → 6 s. A
  100,000-entry folder is compared with the receiver's copy in 0.6 s
  instead of 2.5 s, and its listing is sent compressed.
- **A dropped route recovers sooner.** After a drop, each side goes
  straight back to finding the other instead of retrying the dead route
  twice more.

### Changed

- **A new screen.** The share command stands alone on its line, without
  a box, so a triple-click copies it. A status glyph leads each line
  (`✓ ✗ ! ? → ←`), details are faint, and a summary is the outcome plus
  one line of numbers. Colors are the terminal's own ANSI colors unless
  fsend knows the background (`FSEND_THEME`, `COLORFGBG`, or the
  terminal's answer when asked). The waiting line fits narrow windows.
- **Errors read sentence first**, with the code after it:
  `No sender is waiting on that code. [E002]`. Your own choices print no
  code: Ctrl-C, and "no" or "skip them" at a prompt.
- **"accepted" means accepted.** The sender prints it only once the
  receiver has agreed. Until then it says which device it is waiting
  for, and a decline names the device.
- **Ctrl-C during a receive** says how far it got, where the partial
  data is, and the command that resumes it.
- **Short `--text` prints at once** on the receiver, without a prompt.
- **Failed handshakes no longer end a send.** A sender used to stop with
  `E046` after 10 failed handshakes on its local-network port. Now that
  port accepts only local addresses (loopback, private, link-local and
  on-link ranges), each failure delays the next attempt, and after 10
  the sender stops listening on the local network and keeps waiting
  through the server.
- **A receiver that connects and then vanishes no longer uses up the
  code.** The sender says so and waits again.
- **A broken link inside a folder no longer stops the send.** The sender
  skips it, lists it, and exits `E050`; everything else arrives. A
  broken link named on the command line, or a link that loops, still
  stops with `E036`.
- **A name the receiver can't create skips that file**, not the whole
  transfer: for example `aux.c` or `12:30.txt` sent to Windows. Both
  sides list it and exit `E050`.
- **Keep both doesn't pile up copies.** If `name (1).ext` already holds
  the incoming version, a re-send leaves it alone.
- **`FSEND_SERVER_PASSWORD` from the environment is sent only to a
  server you chose**, never to the public default server. A password
  saved with `--connect` is unchanged.
- **`fsend --update`** finds the latest version without the GitHub API,
  so it no longer fails on the API's hourly limit.
- **Windows `--uninstall`** removes the install folder from `PATH` only
  when fsend was the only thing in it.

### Fixed

- **An interrupted replace resumes.** Replacing an existing file used to
  start from zero after every interruption.
- **Folder modification times** are restored on the receiver.
- **A file that appears at the destination during the transfer** is kept,
  and the incoming one is skipped and listed. It used to be replaced
  without asking.
- **A device with a wrong clock can pair through the server.** Codes are
  salted with the UTC day, so a clock hours off found no sender. Clients
  now follow the server's date when theirs is more than 5 minutes off,
  and `E002` says how far off the clock is.
- **`E002` points at a version mismatch**: 1.15 and older can't reach
  1.16 and later.
- **`fsend doctor` on Linux and Windows** no longer reports local
  discovery as unavailable when only loopback multicast is missing.
- **A sender's Ctrl-C reaches the receiver** as "the sender cancelled"
  (`E032`) even when the first notice is lost, and Ctrl-C works while a
  write is blocked.
- **`install.ps1` no longer changes your PowerShell session** (strict
  mode, error preference, leftover functions) when run with `irm | iex`.
- **`install.sh` keeps the old binary when the copy fails**: the new one
  is staged inside the install folder and renamed into place.
- **Relay: a flood of STUN requests** no longer pushes registered peers
  onto the TCP fallback.
- **The server image** is built with Go 1.27.4, which fixes GO-2026-6859
  in `net/http`.

### Removed

- **`FSEND_STATE_FILE` (server).** A server that still sets it logs a
  warning and ignores it. You can drop the variable and the `fsend_state`
  volume from your compose file.
  - Codes still survive a redeploy: a waiting sender registers its code
    again with the new process. A 1.16 sender run with `--mode direct`
    (server only) doesn't, and gets `E007` on a restart.
  - The relay's daily byte count (`FSEND_RELAY_MAX_BYTES_PER_DAY`) starts
    again on every restart.
- **`E046`** is no longer raised (see Changed).
- **`your_observed_addr` and `peer_observed_addr`** are gone from the
  server's responses. No client has read them since 1.0.

## 1.16.0

- **Protocol 3: update both ends.** Sender and receiver both need 1.16 or
  later. An older peer can't find a 1.16 sender's code (`E002`).
- **Update self-hosted servers with the clients.** A 1.16 server refuses
  clients older than 1.16 (HTTP 426, `E060`), and 1.16 clients can't use
  an older server's relay.

## 1.10.0

- **Symlink entries are rejected on receive.** They closed a path
  traversal. Senders already sent the targets of symlinks as real files,
  so normal transfers are unaffected.

## 1.6.0

- **Protocol 2: update both ends.** Sender and receiver both need 1.6 or
  later. Mixing with 1.0 – 1.5 fails with `E034`.
