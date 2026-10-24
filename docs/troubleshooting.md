# Troubleshooting

Common situations and how to fix them. Every fsend error also prints a
short code like `E014`; this page is about what to *do* about it. For the
full list of codes, see [Usage → Exit codes](usage.md#exit-codes).

Wondering what the server can see, or whether your ISP can read the file?
The short answer is no — everything is end-to-end encrypted between the
two machines. See [Security](security.md) for the details.

## Connecting

### "Server unavailable; retrying — until then only receivers on your local network can connect"

fsend couldn't reach the pairing server. This sender-side warning isn't
fatal: the sender keeps retrying in the background, and **same-Wi-Fi
transfers still work** — they use the local network directly and don't
need the server. Only *cross-network* receivers (someone on a different
network) can't connect until the server is back.

When the server can't be reached at all — no local-network fallback in
play — the run ends with `E001`: "Could not reach the server." The
`Server:` line under it names the cause (timed out, DNS lookup failed,
connection refused, no network route).

Three variants of this warning name a cause you can fix yourself:

- "The server requires a password" — set it with
  `fsend --connect <host:port>,<password>` (ask the server operator).
- "The server rate-limited this device; retrying" — it retries on its
  own; if it persists, wait a minute and run fsend again.
- "The server's certificate isn't trusted (a Wi-Fi login page?)" — see
  [`E047`](#the-servers-certificate-is-not-trusted-e047) below.

What to try:

- Check your internet connection.
- If you run your own server, confirm it's up:
  `curl https://your-server/health`.
- Run `fsend doctor` to see which part fails.
- Point at a different server with `fsend --connect <host>:443` (or
  `--server <host>` for one run), or back to the default with
  `fsend --connect default`.

### The server's certificate is not trusted (`E047`)

```text
✗ [E047] The server's certificate is not trusted.
  A Wi-Fi login page or a filtering proxy may be intercepting the
  connection. Sign in to the network (open any website), or try
  another network.
```

The pairing server answered, but its TLS certificate didn't verify
against your system's trusted roots. That is almost always a captive
portal (hotel or café Wi-Fi before you sign in) or a TLS-intercepting
proxy whose certificate your system doesn't trust. Sign in to the network
(open any website) or switch networks. The relay's TCP fallback goes
through the same HTTPS endpoint, so it can't get around an intercepting
proxy either. A sender shows this as a retrying warning rather than
exiting.

### The code doesn't work on the receiver (`E002`, `E003`, `E004`)

```text
✗ [E003] That code has already been claimed by another receiver.
  Ask the sender to generate a fresh code.
```

| Code | Meaning | Fix |
|---|---|---|
| `E004` | The code isn't a valid `abc-defg-jkm` shape | Re-type it; codes use only `a–z` minus `i`, `l`, `o`. Case, spaces and a missing hyphen are fine. |
| `E002` | No sender is waiting on that code (after about 6 seconds of trying) | It was mistyped, the sender stopped, or it was used more than ten minutes ago. Have the sender run again for a fresh code. |
| `E003` | Another receiver already connected with this code | Each code works for one receiver. (A receiver resuming its own dropped transfer on the same device doesn't hit this.) Have the sender run again and share the new code. |

Codes can't be reused — a new send always produces a new code to share.
After a receiver connects, the server remembers the code as used for ten
minutes (`E003`); after that it no longer knows it (`E002`).

### The sender stopped: "too many failed attempts" (`E046`, sender side)

The sender allows 10 failed handshakes on one code, then stops. A failed
handshake is a receiver using the wrong code against your listener — so
either someone mistyped it ten times, or someone is guessing. Run fsend
again for a fresh code and share it only with the receiver.

### Your code expired before anyone received it (`E007`, sender side)

This one's on the *sender*: a pairing round waits up to an hour, and no
receiver connected in that time. Run the send again for a fresh code and
share it sooner. (The same limit applies while the sender waits for a
dropped receiver to come back.)

### "This version of fsend is too old for the pairing server" (`E060`)

```text
✗ [E060] This version of fsend is too old for the pairing server.
  Update fsend, then try again:
    fsend --update
```

The server answered HTTP 426: either your fsend speaks an older wire
protocol than the server supports, or the operator set a minimum client
version (`FSEND_MIN_CLIENT_VERSION`). Run `fsend --update` (or
`brew upgrade fsend`). The other side needs a compatible version too:
two peers on incompatible protocol versions fail with `E034`.

A sender that hits this keeps its code open for receivers on the same
local network and says so: "This fsend is too old for the server (run
fsend --update)".

### The transfer never connects, even across the internet (`E014`)

fsend tried a direct connection, then the fallback relay, and neither
worked.

- The other side's fsend may have stopped or lost its network.
- The server may run in pairing-only mode (`FSEND_RELAY_ENABLED=false`),
  so there is no relay when hole-punching fails.
- When self-hosting, check that **`443/udp`** reaches your server — many
  setups open TCP and forget UDP. (Server operators: see
  [Self-hosting → Troubleshooting](self-hosting.md#troubleshooting).)

Run `fsend doctor` on both machines to see which part fails.

### The network blocks UDP (`E042`)

```text
✗ [E042] This network seems to block the UDP traffic fsend uses.
  Try another network (a phone hotspot works), or ask the network admin
  to allow outbound UDP port 443. `fsend doctor` checks both paths.
```

Direct transfers and the relay both prefer UDP. When the relay doesn't
answer over UDP within 2 seconds, fsend switches automatically to the
**TCP fallback**: the same relay, reached over a WebSocket on the
server's HTTPS port (TCP 443). The route then shows as `relay`, and it
can be slower than over UDP. `E042` means that fallback failed too, or
the server is too old to offer it.

- Run `fsend doctor`: the `UDP` and `TCP` lines say which path works.
- Try another network (a phone hotspot usually works).
- If you run the server, update it: the TCP fallback needs a server that
  serves `/v1/relay/ws` through its HTTPS proxy (the bundled Caddy does).
- A TLS-intercepting proxy breaks the fallback as well; that usually
  shows up first as [`E047`](#the-servers-certificate-is-not-trusted-e047).

### `fsend doctor` reports a problem (`E043`)

`fsend doctor` exits `43` when any check is degraded. Each `⚠` line (or
`[!]` in plain output) says what it affects and what still works:

| Line | What it means |
|---|---|
| `server … unreachable` | Cross-network transfers fail; same-LAN transfers are unaffected. |
| `UDP` | The relay didn't answer over UDP; relayed transfers will use the TCP fallback. |
| `TCP … no TCP fallback for its relay` | The server predates the fallback; on a network that blocks UDP, cross-network transfers will fail. |
| `mDNS` (LAN answer) | Multicast is blocked here; receivers on other devices will pair through the server instead. |

See [Usage → `fsend doctor`](usage.md#fsend-doctor) for every check.

### "Could not verify the other side" (`E022`)

fsend connected but couldn't confirm the peer is who you expect — the
two sides derived a different secret. Almost always a **mistyped code**:
re-check it character for character and try again. It can also mean an
active tamper attempt on the connection, which fsend refuses to transfer
through (this is the protection working, not a bug).

### "Server closed the connection because no data was flowing" (`E029`)

The connection stalled long enough for the server to drop it. Usually a
flaky network mid-transfer. While the sender is still running, rerun the
same command on the same device — the transfer
[resumes](usage.md#resuming-an-interrupted-transfer) from where it
stopped.

## Passwords

| Code | Situation | Fix |
|---|---|---|
| `E021` | Wrong **transfer** password | Re-enter the sender's `--password` value, or set `FSEND_PASSWORD`. |
| `E031` | The transfer needs a password you didn't supply | The sender used `--password`; receive with `--password` (or `FSEND_PASSWORD`). |
| `E028` | The **server** needs a password | Connect with `fsend --connect host:443,<password>`. See [Self-hosting](self-hosting.md#require-a-password-optional). |

Note the two are different: `--password` protects a *transfer*; the server
password gates a *self-hosted server*.

## Files

### "One or more files differed and were kept" (`E013`)

A file you're receiving already exists locally with **different
contents**, so fsend kept your local copy and didn't overwrite it. The
rest of the transfer completed.

- To replace differing files, receive again with `--overwrite`.
- The accept prompt shows the breakdown (new / up to date / differ) before
  you confirm; `--manifest <file>` records the exact per-file outcome.
- Byte-identical files are always skipped silently — this only fires on
  real differences.
- To keep both copies, receive again with `--rename` (the incoming file
  is saved as `name (1).ext`).
- If you chose "skip them" at the prompt yourself, the line renders as
  the neutral "Kept your local copies" and fsend exits 0 — your choice,
  not a failure. Unattended runs (`--yes`) that keep files still exit 13.
- The sender sees the same outcome: "N files kept by receiver", and
  "Nothing sent" with a warning glyph when no bytes moved at all.

### "Some files were skipped" (`E050`)

```text
✓ Saved 2 of 3 files to ./ — 10 B in 7ms  ·  local network
⚠ 1 file was skipped:
    proj/locked.txt — the sender could not read it
✗ [E050] Some files were skipped — everything else arrived.
  The skipped files and the reason are listed above. Fix the cause and
  run fsend again; files that already arrived are not sent twice.
```

The transfer finished, but some entries didn't make it. Both sides list
them with a reason:

- **The sender couldn't read the file** — permissions, or it vanished
  or shrank while being sent. Fix it on the sender and send again.
- **The name can't be placed on the receiver** — two incoming names
  that your filesystem treats as the same (`Report.pdf` and
  `report.pdf` on a case-insensitive disk, or the NFC and NFD spellings
  of an accented name), or a name reserved for fsend (`.fsend`). Rename
  one on the sender, or receive onto a case-sensitive filesystem.

Re-sending is cheap: files that already arrived are skipped as up to
date.

### "This transfer has too many files and folders" (`E051`)

One transfer is capped at 4,000,000 entries (files plus folders). The
sender checks before issuing a code. Send in parts — one subfolder at a
time — or pack the tree into an archive first
(`tar c dir | fsend --as dir.tar`).

### "File arrived corrupted" (`E011`)

A file's contents didn't match the sender's BLAKE3 root on arrival, so
fsend refused to keep it. Run the transfer again. If a file changed on
the sender while it was being read, it is still saved, with a warning on
both sides that the copy may be inconsistent — send it again once the
source is stable. When resuming, a partial that no longer matches the
source is simply sent again from the start.

### "Path traversal rejected" (`E012`)

fsend blocked a file whose path tried to escape your target directory (a
malformed or malicious listing). Nothing was written outside the target.
If you trust the sender, ask them to re-send; otherwise this is fsend
protecting you.

### Sending stopped on a symlink (`E036`, sender side)

fsend follows symlinks to send their target's content, so a link it can't
resolve halts the send before any code is generated. The message names the
offending link — fix it, or skip it with `--exclude`:

- **Broken** (the target doesn't exist) or **unreadable** (permissions).
- **Cyclic** — a link that loops back into the folder (`a → b → a`, or a
  link to a parent directory), which would otherwise recurse forever.

## Performance

### The transfer is slower than expected

The progress line ends with the route — `local network`, `direct` or
`relay`.

- **A relay is in the path.** When two machines can't connect directly
  (strict NAT/firewall), fsend falls back to relaying through the server,
  which is limited by the server's bandwidth and any caps its operator
  set, rather than by your own links. A relay only kicks in when
  hole-punching fails. See [How it works](architecture.md).
- **The relay runs over TCP.** On a network that blocks UDP, the relay is
  reached over the TCP fallback, which can be slower still.
- **One side's upload is the bottleneck.** A direct transfer can't beat
  the slower of the two links, and high latency or packet loss on the
  path lowers it further.
- **The disk or CPU is.** Many small files each need a sync (batched,
  but not free), `--checksum` reads everything already on disk, and
  resuming hashes the existing partial before it continues.

## Anything else

### "Unexpected error" (`E099`)

A bug or an unhandled condition. Re-run with `--debug` for detail and
please [open an issue](https://github.com/polius/fsend/issues) with the
output.

### Resetting things

- **Back to the public server:** `fsend --connect default`.
- **See which server you're using:** `fsend --connect`.
- **Remove fsend entirely:** `fsend --uninstall` (removes the binary and
  the config directory; `--yes` skips the prompt).
