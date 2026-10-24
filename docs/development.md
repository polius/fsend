# Development

Build and test `fsend` (CLI and server are the same binary; the server
is the `fsend server` subcommand) from source.

## Prerequisites

- **Go ≥ 1.27.3** (matches `go.mod`).
- Optional: **Docker** + **docker compose v2**, only for the reverse-proxy
  stack in [`deploy/compose/`](../deploy/compose/).

## Build

```sh
go build -o /tmp/fsend ./cmd/fsend
```

```sh
/tmp/fsend --version
/tmp/fsend server --help
```

## Tests

```sh
go test ./...           # unit + E2E
go test -short ./...    # skip E2E
go test -race ./...
go test -v ./test/e2e/
```

The E2E suite builds its own binary in a temp dir and runs an isolated
server on loopback — no shell wrapper needed. It takes roughly 70–110 s
(about 105 s on an M-series laptop, including the build); much of that
is tests waiting out real timeouts. CI runs every gating `go test` with
`-count=1 -timeout 600s`, so it never reuses cached results.

## CI

`.github/workflows/checks.yml` runs tests (plus `-race` on amd64 and
arm64), golangci-lint, govulncheck, fuzzing, a dead-code check, a
snapshot release build of every target (checked to be byte-identical on
rebuild), the install-script smoke tests, workflow linting (actionlint,
zizmor) and the coverage gate. A final **`ci-ok`** job fails unless
every other job passed; it is the one check to mark as required on the
branch, so new jobs are gated without changing repository settings.

## Coverage

```sh
scripts/coverage.sh          # report
scripts/coverage.sh --gate   # also fail below the per-package floors (CI runs this)
```

`--gate` fails when a security-critical package drops below its floor:
`internal/transfer` 77 %, `internal/wire` 85 %, `internal/quicconn` 80 %,
`internal/pake` 88 % (`FLOORS` in the script; they sit a few points under
the measured merged coverage).

The script runs unit and E2E tests with coverage and prints per-package
and merged totals. The E2E suite builds `fsend` with
`-cover -coverpkg=./...`, so the orchestration code it exercises —
sender/receiver pairing, ICE, relay — counts toward the number instead of
reading as 0% (which is how per-package profiling reports it). Run the
script for the current total.

## Design tokens

The terminal UI speaks one semantic palette, defined in
`internal/uxlog/glyphs.go`. Six roles, borrowed from the family
opencode's themes use so the CLI vocabulary and the TUI vocabulary stay
parallel:

| role | dark | light | both (default) | means |
| --- | --- | --- | --- | --- |
| accent | `#ff9e64` | `#b15c00` | `#c85000` | act — share code, code box, spinner, direction arrows |
| primary | `#bb9af7` | `#7847bd` | `#8f58dc` | decide — questions, prompts |
| success | `#9ece6a` | `#587539` | `#47851b` | reassure / file references — ✓, paths |
| warning | `#e0af68` | `#8c6c3e` | `#a06b00` | caution — ⚠, "differs" |
| error | `#f7768e` | `#b3261e` | `#da3644` | failure — ✗ |
| info | `#7dcfff` | `#007197` | `#007fa9` | status — ℹ, `--help` flags, links |

Every sibling clears at least WCAG AA (4.5:1) against its background:
**dark** is the pastel family against black, **light** the same hues
darkened against white, and **both** mid-tones that clear 4.5:1 against
white *and* black. One column is chosen per process:

1. `FSEND_THEME=light|dark` — the explicit override.
2. `COLORFGBG` — the `<fg>;<bg>` var iTerm2/Konsole/rxvt export; a light
   background index (7, or 9–15) selects the light column, any other the
   dark one.
3. Otherwise **both** — the common case, since most terminals (macOS
   Terminal included) don't reveal their background.

Truecolour is used when the terminal advertises it
(`COLORTERM=truecolor`/`24bit`). Otherwise dark and light map each hex to
its nearest xterm-256 cell at render time, so the fallback can never
drift from the palette it approximates; the both column uses hand-picked
256-colour cells, and the terminal's default foreground for accent and
warning, where the cube has no cell that clears 4.5:1 on both
backgrounds. `FSEND_PLAIN=1` (or a non-UTF-8 locale) drops colour,
animation and Unicode symbols altogether.

There is deliberately no dim/muted renderer. Secondary text renders in
the terminal's default foreground — the one colour guaranteed readable on
the user's theme — and hierarchy comes from emphasising the primary
elements, never from darkening the rest.

**`brand/logo-orange` is a separate, fixed token: `#E0805A`.** The
wordmark and the README icon use it because a logo may hold a colour the
terminal theme cannot shift; it is a muted, print-safe sibling of `accent`
(`#ff9e64`, ~6:1 on the dark background). Do not "correct" it to the
accent — `docs/assets/icon*.svg` is the reference. The README demo
(`docs/assets/demo.svg` and `demo-light.svg`, produced by `scripts/preview`)
is rendered in the dark and light columns above; a new token needs its
dark → light pair added to `TOKENS` in `scripts/preview/svg-postprocess.py`.

## UX colour audit

```sh
python3 scripts/validate-ux.py
```

Empirical audit of the terminal UI: builds the CLI, starts an isolated
pairing server on loopback (isolated `HOME` — your real config is never
touched), and runs every user-facing scenario as concurrent pty
sessions. Each element's rendered colour is parsed from the raw SGR
stream and asserted against the design tokens in
`internal/uxlog/glyphs.go`; degradation contracts (NO_COLOR, piped
stderr, ASCII fallbacks) must hold byte-clean. Unix only. Run it before
merging any change that touches `internal/uxlog` or the CLI's stderr
surfaces — it is a manual gate, not wired into CI, because its
assertions are design choices (exact hues) that are meant to evolve.


## LAN smoke test (no server needed)

`fsend` uses mDNS for direct peer-to-peer on the same LAN.

Terminal A (sender):
```sh
echo "hello" > /tmp/hello.txt
/tmp/fsend /tmp/hello.txt
# → abc-defg-jkm
```

Terminal B (receiver):
```sh
mkdir -p /tmp/recv && cd /tmp/recv
/tmp/fsend --yes abc-defg-jkm
```

## Run the server locally

For internet transfers (one peer behind NAT), the client needs a pairing
server. To exercise the full client↔server↔client path:

```sh
FSEND_SERVER_ADDR=":18080" \
FSEND_RELAY_ADDR=":18443" \
FSEND_LOG_LEVEL=debug \
/tmp/fsend server
```

Ports `18080`/`18443` are chosen to avoid the privileged-port requirement
of `:443` (would need root) and the common port collision on `:8080`.

For a single run, point a client at it without touching any config:

```sh
FSEND_SERVER=http://127.0.0.1:18080 /tmp/fsend doctor   # checks signaling, UDP relay and TCP fallback
/tmp/fsend report.pdf --server http://127.0.0.1:18080
```

Or save it (this writes to your real config file; see
[Isolated config](#isolated-config) below to use a throwaway one instead):

```sh
/tmp/fsend --connect 127.0.0.1:18080
```

Reset to the public default:

```sh
/tmp/fsend --connect default
```

Health check (the env var tells the probe where to look):

```sh
FSEND_SERVER_ADDR=":18080" /tmp/fsend server --health-check
```

### Common server env vars

| Var | Default | Notes |
|---|---|---|
| `FSEND_LOG_LEVEL` | `info` | `debug` / `info` / `warn` / `error` |
| `FSEND_SERVER_ADDR` | `:8080` | Signaling listener (TCP) |
| `FSEND_RELAY_ADDR` | `:443` | Relay listener (UDP); also the STUN endpoint |
| `FSEND_RELAY_MAX_BYTES_PER_SESSION` | `0` (unlimited) | Per-session relay cap (wire bytes, post-compression). |

Full list: `/tmp/fsend server --help`. For deployment, ports, and all
tunables, see [Self-hosting](self-hosting.md).

## Isolated config

To experiment with `--connect` without overwriting your real config,
point the client at a throwaway config root:

```sh
XDG_CONFIG_HOME=/tmp/fsend-dev /tmp/fsend --connect 127.0.0.1:18080
```

Config lives in the per-OS path listed in
[Usage](usage.md#choosing-a-server---connect).
