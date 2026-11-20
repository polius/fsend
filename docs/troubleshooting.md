# Troubleshooting

Every error prints a code like `E014`. Find it below. The full list is in
[Usage → Exit codes](usage.md#exit-codes).

First step for any connection problem: run `fsend doctor`. It says what
fails and how to fix it.

## Connecting

### Can't reach the server (`E001`)

- Check your internet connection.
- Your own server? Check it: `curl https://your-server/health`.
- Try another server: `fsend --connect default` goes back to the public
  one.

A sender that loses the server keeps retrying. Receivers on the same
network can still connect, because they don't need the server.

### The server's certificate isn't trusted (`E047`)

Usually a Wi-Fi login page (hotel, café) or a company proxy. Open any
website to sign in, or try another network.

### The code doesn't work (`E002`, `E003`, `E004`)

| Code | Meaning | Fix |
|---|---|---|
| `E004` | Not a valid code. | Re-type it. Codes use `a–z` without `i`, `l`, `o`. |
| `E002` | No sender is waiting on it. | Check for typos, or ask the sender to run again. If it says your clock is off, fix the date and time. Also check both sides run 1.16 or later ([compatibility](compatibility.md)). |
| `E003` | Another receiver already used it. | A code works once. Ask the sender for a new one. |
| `E003`, "already being received on this device" | Another fsend on this device is receiving it. | Wait for it, or stop it and try again. |

### No receiver within an hour (`E007`, sender)

Run the send again for a new code.

### Too old for the server (`E060`)

Run `fsend --update` (or `brew upgrade fsend`). The other side needs a
[compatible version](compatibility.md) too.

### Can't reach the other side (`E014`)

Neither a direct connection nor the relay worked.

- The other side may have stopped or lost its network.
- The server may have its relay turned off.
- Your own server? Check that port **443/UDP** is open
  ([Self-hosting](self-hosting.md#troubleshooting)).

Run `fsend doctor` on both machines.

### The network blocks UDP (`E042`)

fsend switches to the relay over TCP on its own. `E042` means that
failed too.

- Try another network. A phone hotspot usually works.
- Your own server? Update it: the TCP fallback needs a recent version.

### Couldn't verify the other side (`E022`)

Almost always a mistyped code. Check it and try again. It can also mean
someone tampered with the connection, and fsend refused to continue.

### Connection dropped (`E029`, `E020`)

Run the same command again on the same device. The transfer
[resumes](usage.md#resuming-an-interrupted-transfer) where it stopped.

### Same network, but the transfer goes through the server

The route shows `direct` or `relay` instead of `local network`. Run
`fsend doctor` on the **sending** machine:

- **`firewall` shows `!`:** the firewall blocks fsend. The row prints
  the fix ([details](usage.md#the-firewall-check)). On Windows, this
  usually means the "allow access?" prompt was cancelled once.
- **`firewall` has no mark:** doctor couldn't read the rules. Run
  `sudo fsend doctor`.
- **Everything passes:** something between the devices blocks them, such
  as Wi-Fi client isolation (guest networks) or a VPN. Try another
  network or a cable.

### "Stopped taking local-network connections after 10 failed attempts"

Something on your network tried to connect without the code. The sender
keeps waiting through the server, so your receiver can still connect.

## Passwords

| Code | Meaning | Fix |
|---|---|---|
| `E021` | Wrong transfer password. | Use the sender's password. |
| `E031` | The transfer needs a password. | Receive with `--password`, or set `FSEND_PASSWORD`. |
| `E028` | The server needs a password. | `fsend --connect host:443,<password>` ([Self-hosting](self-hosting.md#require-a-password-optional)). |

`--password` protects one transfer. A server password protects a
self-hosted server.

## Files

### No input to answer the prompt (`E024`, receiver)

The receive ran without a terminal (cron, CI, `ssh -T`) and without
`--yes`, so nobody could accept. Nothing was declined: the sender is
still waiting. On the same device, run `fsend --yes <code>`.

### Differing files were kept (`E013`)

Some files already existed with different contents, and `--yes` kept
yours. Everything else arrived. The summary says it in one line:

```text
  Accepting (--yes)
! Kept 2 files of yours that differ · 39 up to date · nothing saved
  Run again with --overwrite to replace them, or --rename to keep both. [E013]
```

- To replace them: receive again with `--overwrite`.
- To keep both: receive again with `--rename`.

Choosing "skip them" at the prompt yourself exits 0, not 13.

### Some files were skipped (`E050`)

```text
✓ Saved 2 of 3 files to ~/Downloads/
! 1 file was skipped:
    proj/locked.txt — the sender could not read it
✗ Some files were skipped — everything else arrived. [E050]
```

Both sides list each skipped file and why. Fix the cause and send again;
files that arrived aren't sent twice.

| Reason | Fix |
|---|---|
| The sender couldn't read it | Fix its permissions on the sender. |
| A broken symlink in the folder | Fix or remove the link, or use `--exclude`. |
| Two names the receiver's disk treats as one (`Report.pdf`, `report.pdf`) | Rename one on the sender. |
| A name the receiver can't create (`aux.c` on Windows, over 255 bytes) | Rename it, or send an archive. |
| A file appeared at the destination meanwhile | Receive again to choose what to do. |

### Too many files (`E051`)

The limit is 4,000,000 files and folders. Send in parts, or as an
archive: `tar c dir | fsend --as dir.tar`.

### File arrived corrupted (`E011`)

fsend refused to keep a file that didn't match its hash. Send it again.

### Unsafe path rejected (`E012`)

A file tried to write outside the target folder. Nothing was written
there. fsend is protecting you.

### Stopped on a symlink (`E036`, sender)

The link is broken, unreadable or loops. Fix it, or skip it with
`--exclude`. A broken link inside a folder is skipped instead (`E050`).

## Performance

The progress line ends with the route.

- **`relay`:** the data goes through the server, so its bandwidth is
  the limit. It's used only when no direct path exists.
- **Relay over TCP:** on networks that block UDP. Slower still.
- **`local network`, but slow:** the sender's firewall may block TCP on
  fsend's port, so it uses the slower QUIC route. `fsend --debug` on the
  receiver shows `route: TCP` or `route: QUIC`. Run `fsend doctor` on
  the sender.
- **The slower side's connection** limits a direct transfer.
- **Disk or CPU:** many small files, `--checksum`, and resuming all
  read from disk.

## Anything else

### Unexpected error (`E099`)

A bug. Run again with `--debug` and
[open an issue](https://github.com/polius/fsend/issues) with the output.

### Starting over

- Back to the public server: `fsend --connect default`.
- Which server am I using: `fsend --connect`.
- Remove fsend: `fsend --uninstall`.
