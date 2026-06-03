# Implementation defaults

**Status:** Locked
**Date:** 2026-06-03
**Scope:** Smaller decisions that don't merit their own doc but should not
be invented at coding time. Anything not specified here defaults to "Go
standard library + idiomatic conventions."

---

## Module path

`github.com/polius/fsend` — replace `polius` with the final GitHub org/user
when the repo is created. All cross-references in the docs use the
placeholder for find-replace at repo init.

**Action before first commit:** decide org/user, find-replace `polius` across
the entire repo (docs and code).

## Go version

**Minimum:** Go 1.24 (required for `X25519MLKEM768` hybrid post-quantum TLS
in stdlib).
**Build/test matrix:** 1.24 and 1.25 (latest at the time of writing).
**Module `go` directive:** `go 1.24`.

## Release artifact naming (locked)

Goreleaser default naming, lowercased:

```
fsend_<version>_<os>_<arch>.<ext>
fsend-server_<version>_<os>_<arch>.<ext>
```

- `<version>` — semver without leading `v` (e.g. `0.1.0`)
- `<os>` — `linux`, `darwin`, `windows`, `freebsd`
- `<arch>` — `amd64`, `arm64`, `armv7`, `386`
- `<ext>` — `tar.gz` for unix, `zip` for windows

Plus per-release in the release assets:
- `checksums.txt` — SHA-256 sums, one per line: `<sum>  <filename>`
- `checksums.txt.sig` — cosign signature
- The binary inside the archive is named simply `fsend` (or `fsend.exe`
  on Windows). No version in the binary filename.

`install.sh` reads these exact names; if you change the naming convention,
update `install.sh` to match.

## Build matrix (GitHub Actions)

Targets to build on every tag:

| OS | Arch | Notes |
|---|---|---|
| linux | amd64, arm64, armv7, 386 | Primary distribution |
| darwin | amd64, arm64 | amd64 = older Intel Macs; arm64 = Apple Silicon |
| windows | amd64, arm64, 386 | |
| freebsd | amd64 | best-effort |

Build flags: `-trimpath -ldflags="-s -w -buildid="` for reproducibility +
small binaries.

## Code signing posture for v0.1.0

- **macOS:** unsigned. Users will see a Gatekeeper warning on first run;
  the README documents the `xattr -d com.apple.quarantine` workaround.
  Notarization requires an Apple Developer account ($99/yr) — defer until
  there's user demand.
- **Windows:** unsigned. SmartScreen warning on first run; users click
  through. Code signing requires an EV certificate ($200-500/yr) — defer.
- **Linux:** static binary, no signing concept beyond the cosign signature
  on the release artifact.
- **cosign signing on the release archives:** **yes from day 1.** Sigstore
  is free. Signatures published to the Rekor transparency log. This is the
  cheap, free win.

## Logging library

**`log/slog`** (stdlib, Go 1.21+). Structured logs, zero deps, idiomatic.

- Default output: stderr
- Default format: text (human-readable) when stderr is a TTY; JSON when
  not (so logs piped to a file or aggregator are parseable)
- Default level: `info` (CLI), controlled by `FSEND_LOG_LEVEL` / `--debug`

## Error handling style

- **Wrap with `fmt.Errorf("…: %w", err)`** to preserve the chain.
- **Sentinel errors for catalog entries.** A single `internal/fserrors`
  package exports `ErrServerUnreachable`, `ErrCodeNotFound`, etc. The CLI's
  user-facing error renderer matches on these via `errors.Is` and maps
  them to the error catalog in `docs/ux/help-text.md` (message text +
  exit code).
- **No `panic` in library code.** Panics in `main` or test helpers only.
- **Never include `%v` of an inner error in a user-facing message** unless
  `--debug` is set. Internal details belong in the `DEBUG:` block.

## CLI framework usage (cobra)

- Single root command `fsend` with no subcommands. Cobra is used for the
  flag parsing, completions, and `--help` generation — not for a command
  tree.
- `Use: "fsend [flags] <file|code|->"`
- Auto-generated shell completions wired up under
  `cmd/fsend/completion.go` (cobra provides this for free).
- `--help` output overridden to match `docs/ux/help-text.md` exactly. Cobra's
  default `--help` is verbose; we use a custom template.

## Concurrency model

- **One goroutine per network-bound activity.** Sender has goroutines for:
  (1) signaling, (2) file reading, (3) QUIC writing, (4) progress
  reporting. Bounded buffered channels between them.
- **`context.Context` everywhere.** Every long-running function takes
  `ctx` and respects cancellation. Top-level context is cancelled on
  Ctrl-C (signal handler in `main`).
- **No globals.** State lives on structs passed explicitly.

## Testing strategy

- **Unit tests per `internal/` package**, target 70%+ coverage.
  `internal/pake`, `internal/wire`, `internal/relay` get extra attention.
- **One integration test** in `test/integration/` that spins up server +
  sender + receiver in-process and transfers a 10 MB random file. Asserts
  byte-perfect equality at the end.
- **Property tests** (`testing/quick` or `gopter`) for the wire-protocol
  framer and the chunk format — ensure round-tripping holds for arbitrary
  inputs.
- **No external test infra** for v0.1.0 (no full NAT simulation, no
  cellular emulator). The Pion ICE library has its own NAT tests we trust.

## Linting

`golangci-lint` with the default-on linters plus:
- `gosec` (security)
- `errcheck` (don't ignore returned errors silently)
- `revive` with the `var-naming`, `package-comments`, `exported` rules
- `staticcheck` (catches dead code, unused params, etc.)

CI fails on any lint error. Format with `gofmt -s` and `goimports`.

## CI workflow (GitHub Actions)

Two workflows:

1. **`.github/workflows/test.yml`** — on every push/PR:
   - Lint
   - `go test ./...` on Go 1.24 + 1.25 × Linux + macOS + Windows runners
   - Integration test on Linux only

2. **`.github/workflows/release.yml`** — on tag push (`v*`):
   - Goreleaser: build full matrix, create archives, generate checksums
   - cosign sign the checksums file
   - Publish to GitHub Releases
   - Build + push Docker image for `fsend-server` to `ghcr.io/polius/fsend-server`
   - Update `latest` tag

No publishing to Homebrew, apt repos, or AUR in v0.1.0. The `curl | sh`
flow + manual `docker pull` is the only supported path. Package-manager
distribution can come later when demand is proven.

## Pion ICE configuration

`pion/ice/v4` defaults are mostly fine. Explicit overrides:

```go
agentConfig := &ice.AgentConfig{
    NetworkTypes:    []ice.NetworkType{ice.NetworkTypeUDP4, ice.NetworkTypeUDP6},
    CandidateTypes:  []ice.CandidateType{
        ice.CandidateTypeHost,
        ice.CandidateTypeServerReflexive,
        ice.CandidateTypeRelay, // we relay via our own server when needed
    },
    Urls: []*stun.URI{
        // Use the fsend server itself for STUN-style reflection — no
        // dependency on third-party STUN servers.
        {Scheme: stun.SchemeTypeSTUN, Host: serverHost, Port: 443, Proto: stun.ProtoTypeUDP},
    },
    InsecureSkipVerify:        false,
    DisconnectedTimeout:       refTo(5 * time.Second),
    FailedTimeout:             refTo(15 * time.Second),
    KeepaliveInterval:         refTo(2 * time.Second),
    InterfaceFilter:           nil, // accept all interfaces
    IPFilter:                  nil, // accept all addresses
}
```

(`refTo` is a small helper for taking the address of a duration literal.)

## quic-go configuration

```go
quicConfig := &quic.Config{
    MaxIdleTimeout:        30 * time.Second,
    KeepAlivePeriod:       10 * time.Second, // beats cellular NAT timeouts
    HandshakeIdleTimeout:  10 * time.Second,
    InitialStreamReceiveWindow:     512 * 1024,      // 512 KiB per stream
    MaxStreamReceiveWindow:         6 * 1024 * 1024, // 6 MiB cap
    InitialConnectionReceiveWindow: 1024 * 1024,     // 1 MiB
    MaxConnectionReceiveWindow:     15 * 1024 * 1024,// 15 MiB
    EnableDatagrams:       false, // we use streams, not datagrams
    Allow0RTT:             false, // not worth the replay risk for our use
}
```

## TLS exporter labels (channel binding)

Two distinct labels, both prefixed `EXPORTER-fsend-`:

| Use | Label | Context |
|---|---|---|
| PAKE channel binding | `EXPORTER-fsend-channel-binding-v1` | empty |
| `--pass` HMAC challenge | `EXPORTER-fsend-pwd-challenge-v1` | empty |

The `-v1` suffix is for future protocol evolution; if the label semantics
ever change, bump to `-v2` rather than reusing.

## File permissions written

- Binary install location: `0755`
- Received files (regular): use sender's mode bits, masked by user's umask
- Received directories: `0755` (or sender's mode if more permissive)
- Config file `~/.config/fsend/config.json`: `0600`
- `.fsend-partial` sidecars: `0600` (same protections as the partial data)

## Stdin / stdout filename conventions

- Stdin send: random name `fsend-stdin-<8-char-suffix>` (where suffix is
  `crypto/rand` hex). Receiver can rename with `--out`.
- `--text` send: `fsend-text-<8-char-suffix>.txt`
- Both use suffix randomness over a 32-bit space — collision-resistant
  enough; uniqueness within a directory matters.

## Versioning of internal protocol elements

Three independent version numbers, all locked at `1` in v0.1.0:

| Layer | Where | Initial value |
|---|---|---|
| Wire-protocol frame `ver` byte | `internal/wire` | `0x01` |
| Signaling API URL prefix | `/v1/...` | `1` |
| Relay datagram `v` byte | `internal/relay` | `0x01` |

These can evolve independently; bumping one doesn't require bumping the
others.

## Repository layout (locked)

```
fsend/
├── cmd/
│   ├── fsend/                  # CLI entry point
│   │   ├── main.go
│   │   ├── commands.go
│   │   ├── completion.go
│   │   └── help.go             # custom help template
│   └── fsend-server/           # server entry point
│       └── main.go
├── internal/
│   ├── code/                   # code generation + regex
│   ├── config/                 # XDG config file read/write
│   ├── fserrors/               # sentinel errors + catalog mapping
│   ├── ice/                    # pion/ice wrapper
│   ├── log/                    # slog helpers
│   ├── pake/                   # PAKE interface
│   │   └── spake2/             # gospake2 vendored
│   ├── quic/                   # quic-go wrapper (single-socket transport)
│   ├── relay/                  # UDP relay datagram framing
│   ├── server/                 # signaling HTTP handlers, session table
│   ├── signaling/              # client-side signaling HTTP client
│   ├── transfer/               # the actual file-transfer orchestration
│   ├── update/                 # GitHub release-check
│   └── wire/                   # wire-protocol framing + gob payloads
├── docs/                       # all design docs (already exist)
├── deploy/
│   └── compose/                # reference docker-compose for self-host
│       ├── docker-compose.yml
│       └── Caddyfile
├── test/
│   └── integration/            # end-to-end tests
├── .github/
│   └── workflows/
│       ├── test.yml
│       └── release.yml
├── install.sh                  # public install script
├── Dockerfile                  # for fsend-server image
├── go.mod
├── go.sum
├── LICENSE                     # MIT
├── README.md
├── SECURITY.md
└── PROJECT_SPEC.md
```

This layout is locked; the implementation should match. Add new packages
under `internal/` as needed without moving the existing ones.
