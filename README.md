<p align="center">
  <img src="docs/assets/icon.svg" alt="fsend" width="250">
</p>

<p align="center"><strong>Direct, end-to-end encrypted file transfer.</strong></p>

<p align="center">
  <a href="https://github.com/polius/fsend/actions/workflows/test.yml"><img src="https://github.com/polius/fsend/actions/workflows/test.yml/badge.svg" alt="CI"></a>
  <a href="https://github.com/polius/fsend/releases"><img src="https://img.shields.io/github/v/release/polius/fsend" alt="Release"></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License"></a>
</p>

```bash
# Sender
$ fsend report.pdf
Share this code:  abc-defg-jkm

# Receiver — same command, code instead of file
$ fsend abc-defg-jkm
✓ Saved to ~/Downloads  ·  2.4 MB
```

## About

fsend lets any two computers transfer files **directly** to each other — no accounts, no cloud, no third party storing the file.

- **Peer-to-peer** — bytes go straight from sender to receiver at your own internet speed (relay only as a fallback)
- **End-to-end encrypted** — two independent layers; even the fallback relay never sees the file
- **No ports to open** — works on any network, no router or firewall setup
- **Send anything** — files, folders, multiple at once, stdin streams, or literal text
- **Resumable** — connection drops? Rerun the same code and fsend picks up where it stopped
- **Password-protected** — gate any transfer with `--pass`; receiver supplies it to unlock
- **Post-quantum** — X25519 + ML-KEM-768 (NIST); future quantum computers can't decrypt today's transfer
- **Self-hostable** — same ~20 MB binary, no database
- **Runs anywhere** — single static binary on Linux, macOS, FreeBSD, Windows; x86 and ARM

## Install

```bash
curl -fsSL https://getfsend.alzina.dev | sh
```

Works on Linux, macOS, FreeBSD, and Windows — x86 and ARM.

<details>
<summary>Other install methods</summary>

From source:

```bash
go install github.com/polius/fsend/cmd/fsend@latest
```

Or grab a release binary from [the releases page](https://github.com/polius/fsend/releases).

</details>

## Examples

Send:

```bash
fsend ./project                    # whole folder
fsend a.txt b.txt c.txt            # multiple files
pg_dump mydb | fsend               # piped from stdin
fsend --text "wifi: hunter2"       # literal string
fsend report.pdf --pass            # password-gated
```

Receive:

```bash
fsend abc-defg-jkm                 # confirm before saving
fsend --yes abc-defg-jkm           # skip the confirmation
fsend --out ~/Downloads abc-defg-jkm
```

## Documentation

| | |
|---|---|
| [Usage](docs/usage.md) | Every flag, env var, and exit code |
| [How it works](docs/architecture.md) | The dual-path design and fallback relay |
| [Comparison](docs/comparison.md) | How fsend differs from croc |
| [Security](docs/security.md) | Threat model — what the server can and cannot see |
| [Self-hosting](docs/self-hosting.md) | Run your own pairing + relay server |
| [Development](docs/development.md) | Build and test from source |

## License

MIT
