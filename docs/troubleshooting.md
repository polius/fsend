# Troubleshooting

Common situations and how to fix them. Every fsend error also prints a
short code like `E014`; this page is about what to *do* about it. For the
full list of codes, see [Usage → Exit codes](usage.md#exit-codes).

Wondering what the server can see, or whether your ISP can read the file?
The short answer is no — everything is end-to-end encrypted between the
two machines. See [Security](security.md) for the details.

## Connecting

### "Server unavailable — only receivers on your local network can connect"

fsend couldn't reach the pairing server. This sender-side warning isn't
fatal: **same-Wi-Fi transfers still work** — they use the local network
directly and don't need the server. Only *cross-network* receivers
(someone on a different network) can't connect right now.

When the server can't be reached at all — no local-network fallback in
play — the run ends with `E001`: "Could not reach the server (timeout)."

What to try:

- Check your internet connection.
- If you run your own server, confirm it's up:
  `curl https://your-server/v1/health`.
- Point at a different server with `fsend --connect <host>:443`, or back
  to the default with `fsend --connect default`.

### The code doesn't work on the receiver (`E002`, `E003`, `E004`)

| Code | Meaning | Fix |
|---|---|---|
| `E004` | The code isn't a valid `abc-defg-jkm` shape | Re-type it; codes use only `a–z` minus `i`, `l`, `o`. |
| `E002` | No such code on the server | It expired, was mistyped, the sender stopped, or another receiver already claimed it. Have the sender run again for a fresh code. |
| `E003` | Someone already claimed this code | Codes are one-shot. Have the sender run again and share the new code. |

Codes can't be reused — a new send always produces a new code to share.

### Your code expired before anyone received it (`E007`, sender side)

This one's on the *sender*: you held a code for an hour and no one
received in time, so the server expired it. Run the send again for a
fresh code and share it sooner.

### The transfer never connects, even across the internet (`E014`)

fsend tried a direct connection, then the fallback relay, and neither
worked.

- The most common cause when self-hosting is **`443/udp` blocked at the
  firewall** — many setups open TCP and forget UDP. Open UDP/443 to your
  server. (Server operators: see
  [Self-hosting → Troubleshooting](self-hosting.md#troubleshooting).)
- On a very locked-down network (some corporate or guest Wi-Fi), all
  outbound UDP may be blocked, which fsend needs. Try a different network
  or a VPN.

### "Could not verify the other side" (`E022`)

fsend connected but couldn't confirm the peer is who you expect — the
two sides derived a different secret. Almost always a **mistyped code**:
re-check it character for character and try again. It can also mean an
active tamper attempt on the connection, which fsend refuses to transfer
through (this is the protection working, not a bug).

### "Server closed the connection because no data was flowing" (`E029`)

The connection stalled long enough for the server to drop it. Usually a
flaky network mid-transfer. Re-run — the transfer
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
- If you answered `n` at the overwrite prompt yourself, the line renders as
  the neutral "Kept your local copies" instead of an error — the exit code
  stays 13 either way so scripts can detect the partial.
- The sender sees the same outcome: "N files kept by receiver", and
  "Nothing sent" with a warning glyph when no bytes moved at all.

### "File arrived corrupted" / "source file changed" (`E011`, `E019`)

- `E011` — a file's contents didn't match its hash on arrival; fsend
  refused to keep a corrupted file. Re-run the transfer.
- `E019` — the **source** file changed while sending, so the partial
  download was discarded to avoid mixing old and new data. Re-run once
  the source is stable.

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

- **A relay is in the path.** When two machines can't connect directly
  (strict NAT/firewall), fsend falls back to relaying through the server,
  which is capped by the server's bandwidth rather than your own links.
  Most transfers go direct; a relay only kicks in when hole-punching
  fails. See [How it works](architecture.md).
- **One side's upload is the bottleneck.** Direct transfers run at the
  slower of the two links' speeds.

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
