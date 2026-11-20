# Development

The CLI and the server are one binary. The server is `fsend server`.

## Build

You need Go 1.27 or later (Go fetches the exact toolchain in `go.mod`).

```sh
go build -o /tmp/fsend ./cmd/fsend
/tmp/fsend --version
```

## Test

```sh
go test ./...           # everything, including end-to-end
go test -short ./...    # skip end-to-end
go test -race ./...
```

- The end-to-end tests build their own binary and run a server on
  loopback. They take about 2 minutes.
- Local discovery tests skip without multicast.
  `FSEND_TEST_REQUIRE_MULTICAST=1` makes them fail instead (CI sets it).
- On Linux, loopback has multicast off: `sudo ip link set lo multicast on`.

## Try it by hand

**Same network, no server:** run `/tmp/fsend /tmp/hello.txt` in one
terminal and `/tmp/fsend --yes <code>` in another.

**With a local server:**

```sh
FSEND_SERVER_ADDR=:18080 FSEND_RELAY_ADDR=:18443 FSEND_LOG_LEVEL=debug /tmp/fsend server
```

Then point a client at it for one run:

```sh
FSEND_SERVER=http://127.0.0.1:18080 /tmp/fsend doctor
/tmp/fsend report.pdf --server http://127.0.0.1:18080
```

To try `--connect` without touching your real config, use a throwaway
config folder:

```sh
XDG_CONFIG_HOME=/tmp/fsend-dev /tmp/fsend --connect 127.0.0.1:18080
```

## CI

`.github/workflows/checks.yml` runs on every pull request:

- Tests on Linux, macOS (ARM and Intel), Windows (x86-64 and ARM),
  32-bit x86 and FreeBSD, plus `-race`.
- golangci-lint, govulncheck, fuzzing, a dead-code check.
- A snapshot release build, checked to be byte-identical when rebuilt.
- Install script smoke tests, the terminal UI audit (below), workflow
  linting, and the coverage gate.

The `ci-ok` job passes only if every other job passed. Mark only that
one as required.

## Coverage

```sh
scripts/coverage.sh          # report
scripts/coverage.sh --gate   # also enforce the floors (CI runs this)
```

Floors: `internal/transfer` 77 %, `internal/wire` 85 %,
`internal/quicconn` 80 %, `internal/pake` 88 % (`FLOORS` in the script).

## Releasing

```sh
git tag v1.17.0 && git push origin v1.17.0
```

`release.yml` runs the tests again, then goreleaser publishes. The
release body lists the commit titles since the last tag, plus a footer
(`release.footer` in `.goreleaser.yml`). Edit it on GitHub to add
highlights.

## Terminal UI rules

All stderr output goes through `internal/uxlog`.

**Grid.** The status glyph is in column 0, text starts in column 2, list
rows in column 4. The share command alone sits in column 6, so a
triple-click copies it. Never write leading spaces; use the printer for
the role:

| Printer | Prints |
|---|---|
| `Status(glyph, text)` | A glyph, then text |
| `Line(text)` | A neutral line |
| `LineCommand(label, cmd)` | A label and its command, the command on its own row when narrow |
| `Note(text)`, `NoteWrapped(text)` | Faint detail |
| `Item(text)` | A list row |
| `On(w).Command(text)` | The command to copy |
| `Question(text, options)` | A prompt with choices |
| `Entry(label)` | A prompt for typed text |
| `Blank()`, `Debug(text)` | An empty line; a `--debug` line |

On a terminal, `Status`, `Line`, `Question` and `Entry` break between
words at the right edge, continuing in the row's own column; list rows
fit their name column to the width. A command or path is never broken.
`validate-ux.py` checks after every scenario that no line is wider than
its pty.

`TestGridIsTheOnlyWayToPrint` fails on any other way of printing
(`fmt.Print*`, writes to `os.Stderr`, indented literals, unknown glyphs).

**Glyphs.** `✓` success, `✗` failure, `!` warning, `?` question (every
prompt starts with it), `→` `←` direction, and the braille spinner.
Others (⚠ ℹ ⟳) are missing from common fonts, so don't use them. Plain
mode uses `[OK]`, `[FAIL]`, `[!]`, `[?]`, `[i]`.

**Weights.** Bold for the subject, default for the sentence, faint for
details (sizes, routes, timings, hints).

**Text.**

- Durations always have a space: `188 ms`, `4.8 s`, `1 min 05 s`. Use
  `uxlog.HumanDuration`, never `%s` on a `time.Duration`.
- Measure widths in terminal columns (`uxlog.Width`, `Clip`), not runes.
  CJK and emoji take two.
- A cut list ends "… and N more", and N is never 1 (`uxlog.Shown`).

**Colors.** Six roles:

| Role | Dark | Light | ANSI | Used for |
|---|---|---|---|---|
| accent | `#e0805a` | `#b54c22` | 36 | The share code, spinner, bar, arrows |
| primary | `#bb9af7` | `#7847bd` | 35 | Questions |
| success | `#9ece6a` | `#587539` | 32 | `✓` only |
| warning | `#e5c07b` | `#7d6500` | 33 | `!`, "differs", a stall |
| error | `#f7768e` | `#b3261e` | 31 | `✗` |
| info | `#7dcfff` | `#007197` | 36 | `--help` flags and links |

Green means "it worked", nothing else. The column is picked in this
order:

1. `FSEND_THEME=light|dark`.
2. `COLORFGBG`, if the terminal sets it.
3. Asking the terminal for its background (OSC 11; Unix only).
4. Otherwise, the terminal's own ANSI colors.

Every value passes WCAG AA (4.5:1) on common themes
(`TestPaletteContrast`). The dark accent is the logo's orange,
`#E0805A`; don't change the logo (`docs/assets/icon*.svg`). A new color
also needs its pair in `TOKENS` in `scripts/preview/svg-postprocess.py`.

## Terminal UI audit

```sh
python3 scripts/validate-ux.py            # all scenarios, about 5 minutes
python3 scripts/validate-ux.py s14 s21    # only these
```

It runs every user-facing scenario in a pty, against a local server
with an isolated `HOME`. It checks each element's color and weight, the
grid, the glyphs and the text rules at several widths and themes. Unix
only. CI runs it on Linux and macOS.

It asserts exact design choices, so a UI change updates the script in
the same pull request. A known product bug goes in `EXPECTED_FAILURES`
(reported as `XFAIL`). Once fixed, it reports `XPASS` and fails, so the
entry is removed with the fix.
