<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/icon-dark.svg">
    <img src="docs/assets/icon-light.svg" alt="fsend" width="250">
  </picture>
</p>

<h3 align="center">Private file transfer, straight between two computers.</h3>

<p align="center">One command to send, one to receive. No account, no setup.</p>

<p align="center">
  <a href="https://github.com/polius/fsend/actions/workflows/checks.yml"><img src="https://github.com/polius/fsend/actions/workflows/checks.yml/badge.svg" alt="checks"></a>
  <a href="https://github.com/polius/fsend/releases"><img src="https://img.shields.io/github/v/release/polius/fsend" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License"></a>
  <a href="https://fsend.alzina.dev/health"><img src="https://img.shields.io/website?url=https%3A%2F%2Ffsend.alzina.dev%2Fhealth&up_message=up&down_message=down&label=public%20server" alt="Public server status"></a>
</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#usage">Usage</a> ·
  <a href="#how-it-compares">Comparison</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#security">Security</a> ·
  <a href="#documentation">Docs</a>
</p>

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/demo.svg">
    <img src="docs/assets/demo-light.svg" width="820"
      alt="fsend demo: the sender runs 'fsend backup.zip' and gets a share code; the receiver runs 'fsend <code>' in ~/Downloads, accepts, and the file is saved there.">
  </picture>
</p>

## Why fsend

- **Direct.** On one network or across the internet, the bytes go straight
  from sender to receiver. An encrypted relay is only the fallback.
- **Private.** End-to-end encrypted, with a post-quantum key exchange. The
  server that pairs the two computers never learns the code, the file names or
  the contents.
- **Fast.** About 940 MB/s with both ends on one machine, so in practice your
  network is the limit, not fsend ([numbers below](#how-it-compares)).
- **Resumable.** A dropped transfer continues where it stopped. Re-sending a
  folder moves only what changed.
- **Send anything.** A file, a folder, several at once, a pipe, or a line of
  text. Add `--password` for a second secret.
- **Runs anywhere.** One static binary for Linux, macOS, FreeBSD and Windows,
  on x86 and ARM.

## Install

**Linux, macOS, FreeBSD** (x86 and ARM):

```bash
curl -fsSL https://getfsend.alzina.dev | sh
```

**macOS**, with [Homebrew](https://brew.sh):

```bash
brew install polius/tap/fsend
```

**Windows**, in PowerShell:

```powershell
irm https://getfsend.alzina.dev/windows | iex
```

**Update or remove:** `fsend --update` installs the latest release, and
`fsend --uninstall` removes fsend and its settings. A Homebrew or package
install is left to brew or the package manager.

<details>
<summary>Other install methods</summary>

From source:

```bash
go install github.com/polius/fsend/cmd/fsend@latest
```

Linux packages: each release attaches `.deb`, `.rpm` and `.apk` files.
Install one with `sudo apt install ./fsend_<version>_amd64.deb`,
`sudo dnf install ./fsend-<version>-1.x86_64.rpm` or
`sudo apk add --allow-untrusted fsend_<version>_x86_64.apk`, and update by
installing the next release's package the same way.

Or grab a release binary from [the releases page](https://github.com/polius/fsend/releases).

</details>

<details>
<summary>Tab-completion (optional)</summary>

Add the line for your shell to its rc file (`$PROFILE` on PowerShell):

```bash
eval "$(fsend completion zsh)"                                 # zsh
eval "$(fsend completion bash)"                                # bash
fsend completion fish | source                                 # fish
fsend completion powershell | Out-String | Invoke-Expression   # powershell
```

The deb, rpm and apk packages install completions for bash, zsh and fish
already, and the man pages (`man fsend`). Release archives carry the same
scripts in `completions/` and the pages in `manpages/`.

</details>

## Usage

**Send.** fsend prints a short code to share:

```bash
fsend backup.zip                   # a single file
fsend ./project                    # a whole folder
fsend a.txt b.txt c.txt            # several files at once
pg_dump mydb | fsend               # a stream from stdin
fsend --text "wifi: hunter2"       # a line of text
fsend backup.zip --password        # gated behind a password
```

**Receive.** Run fsend with that code on the other computer:

```bash
fsend abc-defg-jkm                    # save to the current folder
fsend --yes abc-defg-jkm              # skip the confirmation prompt
fsend --out ~/Downloads abc-defg-jkm  # save to a specific folder
fsend --out - abc-defg-jkm | tar x    # stream to stdout (a folder arrives as tar)
```

No fsend on the other computer? The sender also prints a one-line command that
installs it and receives the file.

More in [Usage](docs/usage.md): resuming, scripting with `--json`, and
`fsend doctor` for when something won't connect.

## How it compares

| | **fsend** | **croc** | **magic-wormhole** | **sendme** | **LocalSend** |
|---|:---:|:---:|:---:|:---:|:---:|
| Works across the internet | ✓ | ✓ | ✓ | ✓ | ✗ |
| Direct transfer, whatever the size | ✓ | ✗ | ✗ | ✓ | LAN only |
| Works with only port 443 open | ✓ | ✗ | ✗ | ✓ | — |
| Post-quantum key exchange | ✓ | ✗ | ✗ | ✗ | ✗ |
| No part of the code reaches a server | ✓ | ✗ | ✗ | — | — |
| Strength of the default code | ~45 bits | ~31 bits | 16 bits | — | — |
| Stdin and stdout streaming | ✓ | ✓ | ✗ | ✗ | ✗ |

**Speed**, with both ends on one machine, so it shows each tool's own
overhead and not your network:

| Local network | **fsend** | **croc** | **magic-wormhole** | **sendme** | **LocalSend** |
|---|:---:|:---:|:---:|:---:|:---:|
| One 1 GiB file | 1.2 s | 1.9 s | 21 s | 25 s | **0.7 s** |
| 5,000 files of 20 KiB | **1.7 s** | 9.1 s | 5.8 s | 35 s | 2.3 s |

The same test over the route each tool uses between networks:

| Between networks | **fsend** | **croc** | **magic-wormhole** | **sendme** | **LocalSend** |
|---|:---:|:---:|:---:|:---:|:---:|
| One 1 GiB file | **21 s** | 34 s | **21 s** | 25 s | — |
| 5,000 files of 20 KiB | **2.5 s** | 9.8 s | 5.8 s | 35 s | — |

Every row, the method and the sources are in the
[full comparison](docs/comparison.md).

## How it works

A share code pairs the two computers. fsend then takes the fastest route that
works, with nothing for you to choose:

| Route | When | How |
|---|---|---|
| **Local network** | Both on the same network | They find each other over mDNS. The data never touches a server. |
| **Direct** | Different networks | Both NATs are hole-punched, and the data flows peer to peer over QUIC. |
| **Relay** | No direct path | The server forwards encrypted data it can't read, over HTTPS when UDP is blocked. |

The default server, `fsend.alzina.dev`, is free and best effort. Transfers on
one network work without it, and the same binary lets you
[run your own](docs/self-hosting.md).

<br>

<p align="center">
  <a href="https://pol.alzina.dev/videos/fsend.mp4">
    <img src="docs/assets/video-poster.png" width="820"
      alt="Watch the fsend video: a 65-second walkthrough of how it works">
  </a>
</p>

<p align="center"><em>Sixty-five seconds on pairing and the three routes. Plays as an MP4 in your
browser. Prefer reading? See <a href="docs/architecture.md">Architecture</a>.</em></p>

## Security

- **Encryption:** TLS 1.3 between the two computers, never ending at the
  server.
- **Key exchange:** X25519MLKEM768, a post-quantum hybrid, with no classical
  fallback.
- **Authentication:** CPace, a PAKE that binds the share code to the TLS
  session. A man in the middle, even the server, fails before any data moves.
- **Integrity:** a BLAKE3 hash per file, checked before the file is kept.
- **What the server sees:** IP addresses, the fsend version and a value derived
  from the code. It stores nothing on disk.
- **Releases:** reproducible builds, a Sigstore signature, build attestations
  and an SBOM.

The whole threat model is in [Security](docs/security.md).

## Documentation

| | |
|---|---|
| [Usage](docs/usage.md) | Every flag, env var, and exit code |
| [Architecture](docs/architecture.md) | The three routes, and how fsend picks one |
| [Security](docs/security.md) | Threat model, and what the server can and cannot see |
| [Comparison](docs/comparison.md) | fsend vs croc, magic-wormhole, sendme and LocalSend |
| [Self-hosting](docs/self-hosting.md) | Run your own pairing server |
| [FAQ](docs/faq.md) | Short answers: privacy, routes and ports, speed, updating |
| [Troubleshooting](docs/troubleshooting.md) | Common errors and what to do about them |
| [Version compatibility](docs/compatibility.md) | Which versions work together; release notes on [GitHub Releases](https://github.com/polius/fsend/releases) |
| [Development](docs/development.md) | Build and test from source |

## Contributing

Bug reports and pull requests are welcome. See [Development](docs/development.md)
to build from source, or open an [issue](https://github.com/polius/fsend/issues).

## License

[MIT](LICENSE)
