# fsend

**Truly peer-to-peer file transfer.** Direct, encrypted, relay only as a fallback.

```bash
fsend report.pdf                   # send
fsend abc-defg-jkm                 # receive
```

## Install

```bash
curl -fsSL https://getfsend.alzina.dev | sh
```

Linux, macOS, FreeBSD, and Windows (Git Bash / MSYS2). Or grab a release
binary from [the releases page](https://github.com/polius/fsend/releases).

## Why fsend

- **Direct peer-to-peer, even across the internet.** Hole-punches through both
  NATs with ICE + STUN. Falls back to an encrypted relay only when NAT
  topology blocks the punch — so your transfer is capped by your own bandwidth,
  not someone else's relay.
- **One UDP port.** UDP/443 — the port HTTP/3 uses, allowed almost everywhere.
  Most tools need a TCP range that corporate firewalls block.
- **Two-layer end-to-end encryption.** TLS 1.3 over QUIC **plus** SPAKE2
  (RFC 9382), channel-bound. An attacker has to break both.
- **Post-quantum forward secrecy** by default (X25519 + ML-KEM-768 hybrid).
  Captured today, still safe against tomorrow's quantum computer.
- **Self-hostable.** Same binary, `fsend server`. No database, no per-transfer
  logs, ~20 MB.

## Send anything

```bash
fsend ./project                    # whole folder
fsend a.txt b.txt c.txt            # multiple files
tar c ./build | fsend              # piped from stdin
fsend --text "wifi: hunter2"       # literal string
fsend report.pdf --pass            # password-gated
```

Cancelled transfers resume on the next run.

## Documentation

| | |
|---|---|
| [Usage](docs/usage.md) | Every flag, env var, and exit code |
| [How it works](docs/architecture.md) | The dual-path design, ICE, fallback relay |
| [Comparison](docs/comparison.md) | How fsend differs from croc |
| [Security](docs/security.md) | Threat model — what the server can and cannot see |
| [Self-hosting](docs/self-hosting.md) | Run your own pairing + relay server |
| [Development](docs/development.md) | Build and test from source |

## License

MIT
