# FAQ

- [Is it encrypted?](#is-it-encrypted)
- [What does the server see?](#what-does-the-server-see)
- [Do both sides need fsend?](#do-both-sides-need-fsend)
- [How long does a code last?](#how-long-does-a-code-last)
- [What if the transfer is interrupted?](#what-if-the-transfer-is-interrupted)
- [How is the route chosen?](#how-is-the-route-chosen)
- [Which ports does it use?](#which-ports-does-it-use)
- [How fast is it?](#how-fast-is-it)
- [Why does macOS or Windows warn about the binary?](#why-does-macos-or-windows-warn-about-the-binary)
- [How do I update or uninstall?](#how-do-i-update-or-uninstall)
- [Can I send text or use pipes?](#can-i-send-text-or-use-pipes)
- [Can I run my own server?](#can-i-run-my-own-server)
- [What's different on Windows?](#whats-different-on-windows)

## Is it encrypted?

Yes, end to end. Only the two devices can read the files.

- Every connection uses TLS 1.3 with a post-quantum key exchange.
- The code proves both sides are who they should be. A man in the
  middle, even the server, fails before any data moves.
- The server and relay only ever carry encrypted data.

Whoever has the code can receive, once. For a second secret, add
`--password`. Details: [Security](security.md).

## What does the server see?

It sees each device's IP address and fsend version, and a value derived
from the code (not the code). When it relays, it sees the size and
timing of the encrypted data.

It never sees the code, file names, contents, device names or your
password. It stores nothing on disk and, by default, logs no IP
addresses. Details:
[Security](security.md#what-the-pairing-server-can-see).

Even on one network, the sender registers with the server, so a receiver
elsewhere can find it. `--local` skips the server completely.

## Do both sides need fsend?

Yes, 1.18 or later on both ([compatibility](compatibility.md)). A
receiver without fsend can install it and receive in one line:

```bash
curl -fsSL https://getfsend.alzina.dev | sh -s -- abc-defg-jkm
```

```powershell
iex "& {$(irm https://getfsend.alzina.dev/windows)} -Code abc-defg-jkm"
```

The sender prints both lines, with the real code.

## How long does a code last?

- While the sender runs, for up to an hour per wait (then `E007`).
- For one receiver. After that, others get `E003`.
- Guessing is impractical: about 45 bits, and each guess costs a slow,
  memory-hard computation. Details:
  [Security → Share codes](security.md#share-codes).

## What if the transfer is interrupted?

- Short drops are retried on their own.
- Sender still running: run the same `fsend <code>` again on the same
  device.
- Sender exited: send again (new code) and receive into the same
  folder. Partial data is kept for 7 days.
- Pipes, `--text` and `--out -` can't resume. Run them again.

Details: [Usage → Resuming](usage.md#resuming-an-interrupted-transfer).

## How is the route chosen?

Automatically. fsend tries the local network and the server at the same
time, and uses whichever connects first.

| Route | When |
|---|---|
| `local network` | Both devices on the same network. |
| `direct` | Different networks, connected peer to peer. |
| `relay via <server>` | No direct path. The server forwards the encrypted data. |

Details: [How it works](architecture.md).

## Which ports does it use?

**Across the internet:** you open nothing. Both sides only connect out,
to the server on TCP and UDP 443.

**On the local network:** the sender listens on one port between 50000
and 50999, from local devices only. macOS and Windows ask once whether
fsend may accept connections; say yes. If a firewall blocks it,
transfers still work through the server, just slower. `fsend doctor`
checks this.

**Running your own server:** open 80/TCP, 443/TCP and 443/UDP
([Self-hosting](self-hosting.md#before-you-start)).

## How fast is it?

Usually your network or disk is the limit. fsend's own limits, with both
ends on one machine and 1 GiB of random data:

| Route | macOS (M2 Pro) | Linux |
|---|---|---|
| Local network | ~1,150 MB/s | ~1,430 MB/s |
| Direct between networks | 52–63 MB/s | ~170 MB/s |
| Relay | 26–29 MB/s | ~170 MB/s |

Re-sending a folder moves only what changed. To compare with other
tools, see [Comparison → Speed](comparison.md#speed). Slow transfer? See
[Troubleshooting → Performance](troubleshooting.md#performance).

## Why does macOS or Windows warn about the binary?

The binaries have no Apple or Microsoft code signature. Releases are
signed another way: with Sigstore and GitHub attestations.

- **macOS:** a browser download is blocked by Gatekeeper. The install
  script and Homebrew avoid this. For a manual download, run
  `xattr -d com.apple.quarantine ./fsend`.
- **Windows:** SmartScreen may warn about a browser download. The
  PowerShell installer avoids this.

To check a download is genuine, see
[Security → Release integrity](security.md#release-integrity).

## How do I update or uninstall?

| Installed with | Update | Uninstall |
|---|---|---|
| Install script | `fsend --update` | `fsend --uninstall` |
| Homebrew | `brew upgrade fsend` | `brew uninstall fsend` |
| `.deb` / `.rpm` / `.apk` | Install the new package | `apt remove` / `dnf remove` / `apk del fsend` |
| `go install` | Run `go install github.com/polius/fsend/cmd/fsend@latest` again | Delete the binary |
| Your own server | `docker compose pull && docker compose up -d` | `docker compose down` |

## Can I send text or use pipes?

```bash
fsend --text "wifi: hunter2"            # the receiver prints it
pg_dump mydb | fsend --as db.sql        # send a stream
fsend abc-defg-jkm --out - | tar x      # receive to stdout
```

For scripts, see [Usage → Scripting](usage.md#scripting).

## Can I run my own server?

Yes. The same binary is the server, and a ready Docker Compose setup is
included. See [Self-hosting](self-hosting.md).

## What's different on Windows?

- **Install** in PowerShell: `irm https://getfsend.alzina.dev/windows | iex`.
  No admin rights needed.
- **Config:** `%LOCALAPPDATA%\fsend\config.json`.
- **Firewall:** allow fsend when asked. Otherwise local transfers go
  through the server.
- **File names** like `aux.c` or `what?.txt` can't be created on
  Windows. They're skipped and listed (`E050`).
- **Tab completion:** add
  `fsend completion powershell | Out-String | Invoke-Expression` to
  `$PROFILE`.
