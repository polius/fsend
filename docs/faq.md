# FAQ

Short answers, with links to the page that has the detail.

- [Is it encrypted? Who can read my files?](#is-it-encrypted-who-can-read-my-files)
- [What does the server see?](#what-does-the-server-see)
- [Do both sides need to install fsend?](#do-both-sides-need-to-install-fsend)
- [How long does a code live? What happens after wrong guesses?](#how-long-does-a-code-live-what-happens-after-wrong-guesses)
- [What happens if the transfer is interrupted?](#what-happens-if-the-transfer-is-interrupted)
- [Same network or internet: how is the route chosen?](#same-network-or-internet-how-is-the-route-chosen)
- [Which ports does it use? What about firewalls?](#which-ports-does-it-use-what-about-firewalls)
- [How fast is it?](#how-fast-is-it)
- [Why does macOS or Windows warn about the binary? How do I verify a download?](#why-does-macos-or-windows-warn-about-the-binary-how-do-i-verify-a-download)
- [How do I update or uninstall?](#how-do-i-update-or-uninstall)
- [Can I send text, or pipe data in and out?](#can-i-send-text-or-pipe-data-in-and-out)
- [Can I run my own server?](#can-i-run-my-own-server)
- [What is different on Windows?](#what-is-different-on-windows)

## Is it encrypted? Who can read my files?

Yes, end to end. Only the two devices can read the transfer.

- Every route runs TLS 1.3 between the two devices, with a post-quantum
  hybrid key exchange (X25519MLKEM768) and no fallback to a classical
  one.
- The share code authenticates that connection through a PAKE (CPace)
  bound to the TLS session. A man in the middle, including the pairing
  server or the relay, fails the handshake before any data moves.
- The server and the relay only ever carry ciphertext.

The code is the whole secret. Whoever has it and reaches the sender
first receives the transfer, and then the code is spent. For a second
secret, send with `--password`. Details: [Security](security.md).

## What does the server see?

The pairing server introduces the two devices. It sees:

- a *slot*: a one-way, memory-hard value derived from the code, not the
  code itself;
- each device's public IP address, and the addresses each offers for a
  direct connection (these include LAN addresses);
- the fsend version of each device;
- when it relays, the size and timing of the encrypted traffic.

It never sees the code, file names, sizes, contents, device names or a
`--password`. It keeps pairing state in memory only. At the default log
level it logs no IP addresses and no slots: only startup, shutdown,
errors, and one line per failed request (method, route, status).

A send on the local network still registers with the server, so that a
receiver elsewhere could find it. `--local` skips the server completely.
Separately, fsend asks GitHub at most once a day whether a newer release
exists (only when run in a terminal); `FSEND_NO_UPDATE_CHECK=1` turns
that off. Details:
[Security → What the pairing server can see](security.md#what-the-pairing-server-can-see).

## Do both sides need to install fsend?

Yes. There is no browser receiver. Both sides also need versions that
work together: 1.16 or later on both
([compatibility](compatibility.md)).

A receiver without fsend can install it and receive in one line:

```bash
curl -fsSL https://getfsend.alzina.dev | sh -s -- abc-defg-jkm
```

```powershell
& ([scriptblock]::Create((irm https://getfsend.alzina.dev/windows))) -Code abc-defg-jkm
```

When the sender uses the default server, it prints an install line of
this kind under the share command.

## How long does a code live? What happens after wrong guesses?

- A code works while the sender keeps running. Each wait for a receiver
  lasts up to one hour, then the sender stops with `E007`. Ctrl-C on the
  sender ends the code at once.
- A code works for one receiver. Once a receiver has connected, another
  device that tries the code is told it was already claimed (`E003`).
  About ten minutes later the code is simply not found (`E002`).
- A mistyped code is not found either (`E002`, after a few seconds of
  looking). Each code leads to its own meeting point, so a wrong one
  never reaches the sender.
- Guessing is expensive. A code has about 45 bits, every guess costs a
  64 MiB argon2id computation, and it only works against a sender that
  is waiting right then. The public server also limits new sessions and
  joins per address.
- Something on your network that connects to the sender without the
  code fails the handshake and gets nothing. The sender does not stop;
  it slows down such attempts and can stop listening on the local
  network for that send. The limits are in
  [Security → Share codes](security.md#share-codes).
- With `--password`, a receiver who types the password gets three
  tries per connection, and the sender gives up after three rounds.

## What happens if the transfer is interrupted?

- A short drop (Wi-Fi blip, network change) is retried automatically,
  and the transfer continues from the bytes already received.
- If that doesn't recover and the sender is still running, run the same
  `fsend <code>` again on the same device. Only that device can resume:
  after the first connection both sides switch to a key only they hold.
- After Ctrl-C on the receiver, fsend prints how far it got and the
  command that resumes it.
- If the sender has exited, send again (a new code) and receive into the
  same folder. The partial data is in a hidden `.fsend/` folder there
  and is kept for seven days.
- Before continuing, the partial file is checked against the sender's
  file. If it doesn't match, that file starts again.
- A piped stream, `--text`, and a receive with `--out -` can't resume.
  Run them again.

Details: [Usage → Resuming](usage.md#resuming-an-interrupted-transfer).

## Same network or internet: how is the route chosen?

You don't choose. The sender announces itself on the local network
(mDNS) and registers with the pairing server at the same time. The
receiver asks the local network first and the server 300 ms later.
Whichever pairs first is used. Each summary names the route:

| Route | When | How |
|---|---|---|
| `local network` | Both devices on one network | Straight between them: TLS over TCP, or QUIC if TCP doesn't connect |
| `direct` | Different networks, NATs that can be hole-punched | Straight between them over UDP (QUIC) |
| `relay via <server>` | No direct path (fsend tries for up to 5 seconds) | Through the server, still encrypted end to end: over UDP, or over HTTPS (TCP 443) when UDP is blocked |

If local discovery is blocked (Wi-Fi client isolation, a firewall), the
two devices pair through the server. They may still connect straight
across the LAN, shown as `local network`, but then over QUIC.

`--local` uses the local network only and never contacts the server.
Details: [How it works](architecture.md).

## Which ports does it use? What about firewalls?

You don't open or forward ports for transfers across the internet. Both
sides only make outgoing connections:

- HTTPS (TCP 443) to the pairing server;
- UDP to the server (port 443 on the public server), to find a direct
  path and for the relay, and UDP between the two devices;
- on a network that blocks UDP, everything goes over HTTPS to the
  server.

On the local network:

- both sides use mDNS (UDP 5353, multicast) to find each other;
- the **sender** accepts incoming connections on one port between 50000
  and 50999, derived from the code, for TCP and for UDP. It accepts only
  addresses on local networks.

If a host firewall blocks that port, the receiver can't use the local
route and goes through the server instead. If it blocks only TCP there,
the transfer uses QUIC over UDP, which is much slower on a fast link.
macOS and Windows may ask whether fsend may accept incoming connections
the first time you send; say yes for local transfers. `fsend doctor`
checks, among other things, the server, the relay over UDP and TCP, and
local discovery.

A server operator opens 80/tcp, 443/tcp and 443/udp
([Self-hosting](self-hosting.md#before-you-start)).

## How fast is it?

On a real network, the link and the disks are usually the limit. These
are fsend's own limits, measured with both ends on one machine, 1 GiB of
random data, the receiver discarding the bytes:

| Route | macOS (M2 Pro) | Linux |
|---|---|---|
| Local network (TCP) | about 1,150 MB/s | about 1,430 MB/s |
| Direct between networks | 52–63 MB/s | about 170 MB/s |
| Relay | 26–29 MB/s | about 170 MB/s |

- Data that compresses goes through zstd on several cores (source code:
  about 690 MB/s on the local route, on macOS).
- The public relay is shared and best effort. A relayed transfer is
  limited by that server, not by your link.
- Re-sending a folder moves only the files that are new or changed.

If a transfer is slow, see
[Troubleshooting → Performance](troubleshooting.md#performance).

## Why does macOS or Windows warn about the binary? How do I verify a download?

The binaries carry no Apple Developer ID signature or notarization and
no Microsoft code signature. Each release is signed in another way: its
`checksums.txt` has a Sigstore signature made by the release workflow,
and the archives and binaries have GitHub build attestations.

- **macOS.** A binary downloaded with a browser is quarantined, and
  Gatekeeper refuses to open it. The install script and Homebrew don't
  hit this: `curl` sets no quarantine flag, and the Homebrew cask removes
  it and says so. For a manual download, verify it (below), then run
  `xattr -d com.apple.quarantine ./fsend`.
- **Windows.** SmartScreen warns about a browser-downloaded `fsend.exe`
  ("Windows protected your PC"), and antivirus heuristics sometimes flag
  unsigned programs. The PowerShell installer downloads and checks the
  archive itself. For a manual download, verify it first.

To verify a download:

```sh
# checksums.txt was signed by fsend's release workflow
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/polius/fsend/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# the file matches its line in checksums.txt
sha256sum --ignore-missing -c checksums.txt     # macOS: shasum -a 256 --ignore-missing -c checksums.txt

# or, for an archive or binary, check the build provenance instead
gh attestation verify fsend_<version>_linux_amd64.tar.gz --repo polius/fsend
```

The install scripts check the archive against `checksums.txt`, and its
signature too when `cosign` is installed. The deb, rpm and apk packages
are listed in `checksums.txt` but are not signed as packages. Details:
[Security → Release integrity](security.md#release-integrity).

## How do I update or uninstall?

| Installed with | Update | Uninstall |
|---|---|---|
| An install script (`install.sh`, `install.ps1`) | `fsend --update` | `fsend --uninstall` |
| Homebrew | `brew upgrade fsend` | `brew uninstall fsend` |
| `.deb` | install the new release's `.deb` | `sudo apt remove fsend` |
| `.rpm` | install the new release's `.rpm` | `sudo dnf remove fsend` |
| `.apk` | install the new release's `.apk` | `sudo apk del fsend` |
| `go install` | run `go install github.com/polius/fsend/cmd/fsend@latest` again | delete the binary from `$(go env GOPATH)/bin` |
| A self-hosted server (Docker) | `docker compose pull && docker compose up -d` | `docker compose down` |

- `fsend --update --check` only reports whether a newer release exists.
- `fsend --update` downloads the new release's install script, checks it
  against that release's checksums, and runs it. As root it refuses
  unless `FSEND_ALLOW_ROOT=1` is set.
- `fsend --uninstall` asks first, then removes the binary and fsend's
  config folder. On a Homebrew or package install, `--update` and
  `--uninstall` stop with `E039` and say what to do instead (the table
  above). The package managers leave the config folder in place (its
  path per system is in [Usage](usage.md#choosing-a-server---connect)).

## Can I send text, or pipe data in and out?

```bash
fsend --text "wifi: hunter2"            # the receiver prints it; no file is kept
pg_dump mydb | fsend --as db.sql        # send a stream; the receiver saves it as db.sql
fsend abc-defg-jkm --out - | tar x      # receive to stdout (a folder arrives as tar)
fsend abc-defg-jkm > note.txt           # keep a text payload
```

Without `--as`, a piped stream is saved as `fsend-stdin-<random>`. For
scripts, `--quiet` prints only the code on stdout and `--json` prints
events as NDJSON. Details: [Usage](usage.md#scripting).

## Can I run my own server?

Yes. The same binary is the server (`fsend server`), and the repository
ships a Docker Compose stack with Caddy for TLS. Clients switch with
`fsend --connect host:443`, or use `--server host` for one run. The
server stores nothing on disk, can require a password, and can run
without the relay. You only need it for transfers between networks: on
one network, fsend works with no server at all. Details:
[Self-hosting](self-hosting.md).

## What is different on Windows?

- **Install** in PowerShell with
  `irm https://getfsend.alzina.dev/windows | iex`. It installs to
  `%LOCALAPPDATA%\Programs\fsend` for the current user and needs no
  administrator rights. Builds exist for x86-64, ARM64 and 32-bit x86.
- **Config** is in `%LOCALAPPDATA%\fsend\config.json`.
- **Firewall.** When Windows Defender Firewall blocks incoming
  connections to fsend, a receiver on the same network finds the sender
  but can't connect to it, and falls back to the server after about 2
  seconds. Allow fsend on private networks to keep local transfers
  local.
- **File names.** Windows can't create some names that other systems
  allow: `aux.c`, `con.txt`, names with `< > : " | ? * \`, or a trailing
  dot or space. A Windows receiver skips those files, lists them, and
  receives the rest (`E050`).
- **Permissions.** Windows keeps only the read-only bit of a file's
  mode.
- **Tab completion:** add
  `fsend completion powershell | Out-String | Invoke-Expression` to
  `$PROFILE`.
- **`fsend --uninstall`** deletes the binary after fsend exits, and
  removes the install folder from `PATH` when fsend was the only thing
  in it.
