# README demo

This directory generates the animated demo at the top of the repo README
(`docs/assets/demo.svg`) — a scripted, fully local fsend transfer shown as two
terminals (sender + receiver) side by side.

Everything runs **locally and offline**: a loopback-only `fsend server`, a
throwaway config/home, and a `/dev/urandom` payload. Nothing touches your real
fsend config, and no real network or pairing server is involved.

The demo is an **animated SVG** (not a WebP/GIF): it's vector, so it stays
razor-sharp at any size and on any display density, it's a fraction of the size,
and — because it animates via CSS rather than decoding hundreds of raster frames
— it's cheap to play even on weak mobile CPUs.

This file is the complete runbook: with it, the demo can be regenerated from
scratch with no other context.

---

## TL;DR — regenerate

```bash
# 1. one-time dependencies
brew install tmux                        # the two-pane stage
npm install -g svg-term-cli              # cast -> animated SVG (needs node)
pip install fonttools brotli             # subset + embed JetBrains Mono (woff2)
brew install --cask font-jetbrains-mono  # the embedded terminal font

# 2. regenerate docs/assets/demo.svg
scripts/demo/record-svg.sh
```

`record-svg.sh` is the single entry point. It builds fsend, records the scripted
transfer, renders the SVG, embeds the font, and frames it. Re-run it any time the
CLI output or styling changes.

> Note for headless/CI runs: recording drives a local loopback transfer over a
> PTY. If it fails to start a session, run it with normal local-network access.

---

## The files

| file | role |
|------|------|
| `demo.sh`                  | Sets up the isolated session and **drives** the two terminals (the "director"). |
| `ptyrec.py`                | Records the session to an asciinema **cast** at a fixed 100x28 PTY size. |
| `catppuccin-mocha.xresources` | The terminal **palette** svg-term applies to the cast. |
| `svg-postprocess.py`       | Renders the cast with svg-term, then adds the **hold, embedded font, and chrome**. |
| `record-svg.sh`            | Orchestrates: build → record → render → write `docs/assets/demo.svg`. |
| `README.md`                | This runbook. |

Output: `docs/assets/demo.svg`, embedded in the repo README via
`<img src="docs/assets/demo.svg" width="820">`.

---

## The pipeline, end to end

```
record-svg.sh
  ├─ demo.sh setup            (off-camera: build fsend, loopback server, panes)
  ├─ ptyrec.py demo.cast      (records demo.sh play at a fixed 100x28 PTY)
  │     └─ demo.sh play       (attach; the director types the transfer)
  └─ svg-postprocess.py demo.cast docs/assets/demo.svg
        ├─ svg-term           (cast -> animated SVG, Catppuccin palette)
        ├─ extend hold        (rescale @keyframes: final frame holds 5s)
        ├─ embed fonts        (JetBrains Mono Regular+Bold, subset, woff2 data-URI)
        └─ add chrome         (coral rounded frame + macOS traffic lights)
```

### 1. `demo.sh setup` — the stage

Builds fsend (or reuses `$FSEND_BIN`), starts an isolated loopback `fsend server`,
lays out two equal `tmux` panes (sender on top, receiver below) with coral card
headers, and writes a non-compressible `/dev/urandom` payload so the transfer
shows a real size/throughput. A `fsend` shim on the panes' `PATH` pins the
transfer to the direct P2P path (`--mode=direct`) so the recording is
deterministic and leaks no hostname, while the on-screen command stays a plain
`fsend …`. (See the comments in `demo.sh` for the full rationale.)

### 2. `ptyrec.py` — the recorder

svg-term needs an asciinema cast, and the demo is a **full-screen tmux** session
that must record at an exact 100x28 geometry. `ptyrec.py` is a ~40-line PTY
recorder that forces that window size (so both panes render fully) and streams a
v2 cast — `asciinema rec` can't fix the geometry headlessly, and `termtosvg`
can't capture a full-screen tmux app at all. `record-svg.sh` runs the director in
the background and kills the session a moment after "Saved" appears, so the cast
ends on the completed transfer.

### 3. `svg-postprocess.py` — render + finish

- **svg-term** renders the cast to an animated SVG, applying
  `catppuccin-mocha.xresources` as the 16-colour palette + background.
- **Hold:** svg-term has no "pause on the last frame" option, so we rescale its
  `@keyframes` timeline to hold the final "Saved" frame ~5s before looping
  (content timing is preserved; only the tail grows).
- **Font:** JetBrains Mono (the fsend wordmark font) Regular + Bold are subset to
  the glyphs actually used and inlined as `woff2` data-URIs, so the demo renders
  in the right font even where it isn't installed.
- **Chrome:** a coral rounded outer frame and larger macOS traffic lights are
  drawn as static vector shapes around the untouched animation.

Every assumption about svg-term's output is asserted, so a future svg-term
version that changes its format fails loudly here instead of silently shipping a
broken demo.

---

## Why these choices (so they aren't re-litigated)

- **SVG, not WebP/GIF.** Vector stays crisp at every size and pixel density with
  no per-frame raster decode, so it sidesteps the resolution/retina trade-offs a
  raster demo forces and plays smoothly on weak mobile CPUs. It's also far
  smaller and themable as text.
- **Embedded font, not a font stack.** GitHub viewers won't have JetBrains Mono
  installed; embedding a subset (~40 KB) keeps the wordmark font everywhere.
  External `@font-face` URLs are blocked in GitHub's `<img>`-rendered SVG, so the
  font **must** be inlined as a data-URI.
- **No `<script>`, CSS-only animation.** GitHub renders the README SVG as an
  image and strips scripts; svg-term's CSS-keyframe animation is what survives.
- **Post-processing, not a custom renderer.** svg-term is the de-facto tool for
  this; the hold/font/chrome it can't do are layered on top, guarded by
  assertions rather than re-implemented.
- **`ptyrec.py`, not asciinema/termtosvg.** `termtosvg` can't capture a
  full-screen tmux session (it records empty), and `asciinema rec` won't fix the
  PTY geometry headlessly. A tiny PTY recorder does exactly what's needed.

---

## Preview without recording

Watch the choreography live in your own terminal (no file produced):

```bash
scripts/demo/demo.sh
```

`Ctrl-b d` to detach, or just close the terminal — the throwaway server and tmux
session are cleaned up on exit.

---

## Tuning knobs

| want to change | where |
|----------------|-------|
| Pacing / the share-code handoff | `director()` in `demo.sh` (the `sleep`s) |
| Pane labels, colours, layout | `do_setup()` in `demo.sh` |
| Terminal size (columns x rows) | `COLS, ROWS` in `ptyrec.py` (and `--width/--height` in `svg-postprocess.py`) |
| Colour theme | `catppuccin-mocha.xresources` |
| End-of-loop pause | `HOLD_S` in `svg-postprocess.py` |
| Coral border / window radius | `MARGIN`, `WIN_R` in `svg-postprocess.py` |
| Traffic-light size / position | `DOT_R`, `DOTS`, `DOT_CY` in `svg-postprocess.py` |
| Embedded font | `find_font()` in `svg-postprocess.py` |

---

## Troubleshooting

- **`record-svg.sh` exits "missing required tool".** Install the dependency it
  names (see TL;DR). `svg-term` is an npm global; `fonttools`/`brotli` are pip.
- **`svg-postprocess.py` raises an `AssertionError`.** svg-term's output format
  changed (new version) and a frame/window/keyframe pattern no longer matches.
  Pin svg-term, or update the matching assertion in `svg-postprocess.py`.
- **The font looks like plain monospace on GitHub.** The embedded data-URI font
  didn't load — confirm `@font-face` is present in `docs/assets/demo.svg` and
  that it's a `data:` URL (not external).
- **Headers scroll off / a pane is clipped.** A pane is shorter than its content;
  check the `even-vertical` split in `do_setup` and the `ROWS` in `ptyrec.py`.
- **Transfer shows a LAN-discovery notice or the wrong path.** Confirm the
  `fsend` shim (with `--mode=direct`) is on the panes' `PATH`.
