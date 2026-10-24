<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/icon-dark.svg">
    <img src="docs/assets/icon-light.svg" alt="fsend" width="250">
  </picture>
</p>

<p align="center"><strong>Private file transfer, straight between two computers.</strong></p>

<p align="center">
  <a href="https://github.com/polius/fsend/actions/workflows/checks.yml"><img src="https://github.com/polius/fsend/actions/workflows/checks.yml/badge.svg" alt="checks"></a>
  <a href="https://github.com/polius/fsend/releases"><img src="https://img.shields.io/github/v/release/polius/fsend" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License"></a>
  <a href="https://fsend.alzina.dev/health"><img src="https://img.shields.io/website?url=https%3A%2F%2Ffsend.alzina.dev%2Fhealth&up_message=up&down_message=down&label=public%20server" alt="Public server status"></a>
</p>

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/demo.svg">
    <img src="docs/assets/demo-light.svg" width="820"
      alt="fsend demo: the sender runs 'fsend backup.zip' and gets a share code; the receiver runs 'fsend <code>' in ~/Downloads, accepts, and the file is saved there.">
  </picture>
</p>

<p align="center"><em>One command to send, one to receive.</em></p>

## Why fsend

`fsend` moves a file from one computer to another with as little ceremony as
possible. No account, no upload step, no ports to open — just two commands and a
short share code.

- **Direct, with no network setup** — on the same network or across NATs that
  can be hole-punched, the bytes go straight from sender to receiver. When no
  direct path exists, or a network blocks UDP, a relay over HTTPS (TCP 443)
  takes over. Nothing to configure.
- **End-to-end encrypted** — TLS 1.3 between the peers, authenticated by the
  share code through a PAKE (CPace) bound to the TLS session, over a
  post-quantum hybrid key exchange (X25519MLKEM768). Even the relay only ever
  sees ciphertext.
- **Send anything** — a file, a folder, several at once, a stdin stream, or a
  literal string.
- **Resumable** — if a transfer drops, the next send picks up where it left
  off, and re-sending a folder moves only the new or changed files.
- **Optional password** — gate any transfer with `--password`.
- **Runs anywhere** — one static binary for Linux, macOS, FreeBSD and Windows,
  on x86 and ARM. Self-host the pairing server if you like.

## Install

**Linux, macOS, FreeBSD** — x86 and ARM:

```bash
curl -fsSL https://getfsend.alzina.dev | sh
```

**macOS** — if you prefer [Homebrew](https://brew.sh):

```bash
brew install polius/tap/fsend
```

**Windows** — run this in PowerShell:

```powershell
irm https://getfsend.alzina.dev/windows | iex
```

Releases are signed and verified on install — see
[Security → Release integrity](docs/security.md#release-integrity).

<details>
<summary>Other install methods</summary>

From source:

```bash
go install github.com/polius/fsend/cmd/fsend@latest
```

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

</details>

## Examples

Send a file, a folder, several at once, a stdin stream, or a literal string:

```bash
fsend backup.zip                   # a single file
fsend ./project                    # a whole folder
fsend a.txt b.txt c.txt            # several files at once
pg_dump mydb | fsend               # a stream from stdin
fsend --text "wifi: hunter2"       # a literal string
fsend backup.zip --password        # gated behind a password
```

…and to receive:

```bash
fsend abc-defg-jkm                    # save to the current folder
fsend --yes abc-defg-jkm              # skip the confirmation prompt
fsend --out ~/Downloads abc-defg-jkm  # save to a specific folder
fsend --out - abc-defg-jkm | tar x    # stream to stdout (a folder arrives as tar)
```

## How it works

<p align="center">
  <a href="https://getfsend.alzina.dev/video.mp4">
    <img src="docs/assets/video-poster.png"
      alt="Watch the fsend video — a one-minute walkthrough of how it works">
  </a>
</p>

<p align="center"><em>One minute: pairing with a share code, then the local, direct and relay
routes fsend races to move the bytes. Plays as an MP4 in your browser; prefer reading?
See <a href="docs/architecture.md">Architecture</a>.</em></p>

## Documentation

| | |
|---|---|
| [Architecture](docs/architecture.md) | Three transfer modes, raced concurrently — and why |
| [Security](docs/security.md) | Threat model — what the server can and cannot see |
| [Usage](docs/usage.md) | Every flag, env var, and exit code |
| [Self-hosting](docs/self-hosting.md) | Run your own pairing server in three steps |
| [Development](docs/development.md) | Build and test from source |
| [Troubleshooting](docs/troubleshooting.md) | Common errors and what to do about them |
| [Comparison](docs/comparison.md) | fsend vs croc, magic-wormhole, sendme and LocalSend |

## Contributing

Bug reports and pull requests welcome — see [Development](docs/development.md) to build from source, or open an [issue](https://github.com/polius/fsend/issues).

## License

[MIT](LICENSE)
