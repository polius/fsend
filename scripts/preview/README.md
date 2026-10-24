# README demo

This directory generates the animated demo at the top of the repo README
(`docs/assets/demo.svg`, dark, and `docs/assets/demo-light.svg`) — a scripted,
fully local fsend transfer shown as two terminals (sender above receiver), in
fsend's own colour tokens — plus raster **`demo.mp4`** and
**`demo.gif`** exports of the same animation for places that don't render SVG
(Reddit, chat apps, social posts).

Everything runs **locally and offline**: a loopback-only `fsend server`, a
throwaway config/home, and a `/dev/urandom` payload. Nothing touches your real
fsend config, and no real network or pairing server is involved.

The README demo is an **animated SVG** (not a WebP/GIF): it's vector, so it stays
razor-sharp at any size and on any display density, it's a fraction of the size,
and — because it animates via CSS rather than decoding hundreds of raster frames
— it's cheap to play even on weak mobile CPUs. The `.mp4`/`.gif` are **derived
from that SVG**, not recorded separately, so they always match it.

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

# 2. regenerate docs/assets/demo.svg + demo-light.svg  (the README assets)
scripts/preview/record-svg.sh

# 3. (optional) regenerate the raster exports from the dark SVG
pip install websocket-client             # drives headless Chrome over CDP
python3 scripts/preview/render-video.py     # -> docs/assets/demo.{mp4,gif}
```

`record-svg.sh` is the single entry point for the SVGs: it builds fsend from
this checkout (so the demo shows the current, possibly unreleased, output),
records the scripted transfer once, and renders it in both themes, embedding the
font and framing each. Re-run it
any time the CLI output or styling changes — then re-run `render-video.py` to
refresh the `.mp4`/`.gif` (it just re-bakes whatever `demo.svg` currently is, so
it needs **none** of the recording deps above — only Chrome + ffmpeg).

> Note for headless/CI runs: recording drives a local loopback transfer over a
> PTY. If it fails to start a session, run it with normal local-network access.

> **Check the throughput before you commit.** The transfer's real speed is
> printed on screen ("100 MB in 2.0s (49.7 MB/s)"). The direct path runs over
> the machine's LAN address, not 127.0.0.1, so a VPN's split-tunnel / transparent
> proxy or a network filter can throttle it to single-digit MB/s — the demo then
> advertises a slow tool and the progress phase drags. Pause such software while
> recording.

---

## The files

| file | role |
|------|------|
| `demo.sh`                  | Sets up the isolated session and **drives** the two terminals (the "director"). |
| `ptyrec.py`                | Records the session to an asciinema **cast** at a fixed 88x33 PTY size. |
| `svg-postprocess.py`       | Themes the cast, renders it with svg-term, then adds the **grid fixes, cursor, hold, fade, embedded font, and chrome**. |
| `record-svg.sh`            | Orchestrates: build → record → render → write both `docs/assets/demo*.svg`. |
| `render-video.py`          | Re-bakes `demo.svg` into `demo.mp4` + `demo.gif` via headless Chrome. |
| `README.md`                | This runbook. |

Outputs:
- `docs/assets/demo.svg` (dark) and `docs/assets/demo-light.svg` — embedded in
  the repo README as a `<picture>` that follows the viewer's GitHub theme, the
  same way the icon above it does.
- `docs/assets/demo.mp4` (~1.2 MB) — H.264 at 2x, the file to upload where SVG
  isn't rendered.
- `docs/assets/demo.gif` (~1.7 MB) — 1x fallback for places that take only GIF.

---

## The SVG pipeline, end to end

```
record-svg.sh
  ├─ demo.sh setup            (off-camera: build fsend, loopback server, panes)
  ├─ ptyrec.py demo.cast      (records demo.sh play at a fixed 88x33 PTY)
  │     └─ demo.sh play       (attach; the director types the transfer)
  └─ svg-postprocess.py --theme dark|light demo.cast docs/assets/demo[-light].svg
        ├─ recolor cast       (light: map every dark token hex to its light sibling)
        ├─ svg-term           (cast -> animated SVG on the theme's base colours)
        ├─ clip frames        (each frame gets its own viewport: no cross-frame bleed)
        ├─ fit grid           (snap run starts to the column grid)
        ├─ exact pitch        (font size 5/3: every glyph advances exactly 1 column)
        ├─ style cursor       (one-cell block, shown only at prompts and questions)
        ├─ extend hold        (rescale @keyframes: final frame holds 6s)
        ├─ add motion         (fade across the loop seam; reduced-motion still)
        ├─ add chrome         (title bar + coral frame + macOS traffic lights)
        └─ embed fonts        (JetBrains Mono Regular+Medium+Bold, subset, woff2)
```

### 1. `demo.sh setup` — the stage

Builds fsend (or reuses `$FSEND_BIN`), starts an isolated loopback `fsend server`,
lays out two `tmux` panes (sender on top, receiver below) with coral card
headers, sets `FSEND_THEME=dark` + `COLORTERM=truecolor` so fsend paints its
exact dark-column hexes, and writes a non-compressible `/dev/urandom` payload so the transfer
shows a real size/throughput. A `fsend` shim on the panes' `PATH` pins the
transfer to the direct P2P path (`--mode=direct`) so the recording is
deterministic and leaks no hostname, while the on-screen command stays a plain
`fsend …`. (See the comments in `demo.sh` for the full rationale.)

### 2. `ptyrec.py` — the recorder

svg-term needs an asciinema cast, and the demo is a **full-screen tmux** session
that must record at an exact 88x33 geometry. `ptyrec.py` is a ~40-line PTY
recorder that forces that window size (so both panes render fully) and streams a
v2 cast — `asciinema rec` can't fix the geometry headlessly, and `termtosvg`
can't capture a full-screen tmux app at all. `record-svg.sh` runs the director in
the background and kills the session a moment after "Saved" appears, so the cast
ends on the completed transfer.

### 3. `svg-postprocess.py` — render + finish

- **Theme:** the terminal base is Tokyo Night (dark) / Tokyo Night Day
  (light) — the family fsend's six colour roles come from — and every colour in
  the output is one of `internal/uxlog/glyphs.go`'s tokens. For the light SVG,
  `recolor_cast()` maps each dark hex in the cast to its light sibling via
  `TOKENS` before svg-term runs, so one recording yields both variants. A colour
  missing from `TOKENS` fails the run rather than shipping unmapped.
- **Frame clip + grid:** svg-term animates a filmstrip of side-by-side frames
  spaced exactly one terminal width apart, but lays its text out on a
  1.002-unit column pitch (font size 1.67, its metric rounded to three
  decimals). Long runs drift off the grid — the code box's right edge missed
  its corners by a visible step — and a line that fills every column (the mpb
  progress bar) spills onto the left edge of the next frame as a stray `:`.
  `fit_grid()` snaps every run's start onto integer columns, `exact_pitch()`
  sets the font size to 5/3 so each glyph advances exactly one column, and
  `clip_frames()` gives each frame its own viewport so no renderer can bleed one
  frame into the next.
- **Cursor:** a one-cell block in the theme's foreground. fsend never hides the
  cursor, so in a real terminal it also parks after each spinner line and at
  column 0 under the progress bar; `style_cursor()` keeps it only on rows with a
  shell prompt (`$`) or a question (`?`, the `[Y/n]` answer) — where someone is
  typing.
- **Hold:** svg-term has no "pause on the last frame" option, so we rescale its
  `@keyframes` timeline to hold the final "Saved" frame 6s before looping
  (content timing is preserved; only the tail grows). The hold is measured from
  the first frame that already looks final, not from svg-term's last stop.
- **Motion:** the loop fades out over the hold's last 0.8s and back in over
  0.4s instead of cutting from "Saved" to an empty screen. The fade runs on the
  terminal content only, so the window chrome stays put. Under
  `prefers-reduced-motion`, the SVG shows the finished transfer as a still.
- **Chrome:** a macOS title bar — traffic lights plus the `> fsend_` wordmark,
  mirroring the explainer video's title-bar style — and a coral rounded outer
  frame are drawn as static vector shapes around the untouched animation.
- **Font:** JetBrains Mono (the fsend wordmark font) Regular + Medium + Bold are
  subset to the glyphs actually used and inlined as `woff2` data-URIs, so the
  demo renders in the right font even where it isn't installed. Subsetting runs
  after the chrome step, so the subset also covers the title-bar text.

Every assumption about svg-term's output is asserted, so a future svg-term
version that changes its format fails loudly here instead of silently shipping a
broken demo.

---

## The raster exports (mp4 + gif)

`render-video.py` turns the finished `demo.svg` into video **without
re-recording** — the SVG stays the single source of truth, so the exports can
never drift from it. It does **not** use the recording deps (tmux, svg-term,
fonttools); it only needs Google Chrome, ffmpeg, and the `websocket-client`
python package.

```
render-video.py docs/assets/demo.svg
  ├─ parse              (read width/height, animation-duration, @keyframes stops)
  ├─ headless Chrome    (load the SVG on a flat backdrop; CDP over a websocket)
  │     ├─ device metrics   (exact viewport at 2x scale — supersample the text)
  │     ├─ pause animation  (Web Animations API: el.getAnimations()[0].pause())
  │     └─ scrub + shoot     (set currentTime per fps tick; screenshot each stop)
  └─ ffmpeg
        ├─ demo.mp4     (libx264 -crf 18, yuv420p, +faststart, 1918x1630 @30fps)
        └─ demo.gif     (global palette, dither=none, half res @12fps)
```

### How it stays sharp **and** steady

The two problems with "SVG → video" are blurry text and a trembling image; the
script is built to avoid both:

- **Deterministic scrub, not a real-time screencast.** Chrome's screencast emits
  frames only when something changes, at irregular timestamps; resampling that to
  a fixed frame rate makes the text edges shimmer. Instead we **pause** the SVG's
  CSS animations (the filmstrip and the loop fade) and **seek** `currentTime` to
  evenly spaced ticks, screenshotting each. Held frames come out byte-identical
  and exactly `1/FPS` apart, so the image is rock-steady and x264/GIF dedupe the
  repeats (which is why the files stay tiny). We only screenshot once per
  distinct `@keyframes` stop and reuse it for the held ticks; only the fade
  spans, where every tick differs, are shot tick by tick.
- **2× supersample.** Capturing at `deviceScaleFactor=2` (→ 1918×1630) renders
  the small terminal text at double density; encoding from that keeps it crisp
  where a 1× capture would look soft under chroma subsampling.
- **Exact viewport.** `Emulation.setDeviceMetricsOverride` forces the viewport to
  the SVG's exact size — headless Chrome otherwise picks a shorter height and
  clips the bottom pane.

### mp4 vs gif

- **`demo.mp4`** is the one to share: H.264 / `yuv420p` / even dimensions plays
  everywhere (Reddit converts it to native video), and it's ~1.2 MB.
- **`demo.gif`** is the fallback for the few places that accept only GIF. It's
  exported at **half the capture resolution (1x)** with a global 256-colour
  palette and `dither=none` (the terminal art is flat colour, so error-diffusion
  would only add grain). The demo's two live progress bars repaint the full width
  of the frame every ~80 ms, so a full-2× gif balloons past 12 MB for no real
  legibility gain — at 1x it lands around 1.7 MB. Always build the gif from the
  clean PNG frames, never from the mp4: the mp4's chroma noise wrecks
  inter-frame compression and balloons it several-fold.

---

## Why these choices (so they aren't re-litigated)

- **SVG, not WebP/GIF, for the README.** Vector stays crisp at every size and
  pixel density with no per-frame raster decode, so it sidesteps the
  resolution/retina trade-offs a raster demo forces and plays smoothly on weak
  mobile CPUs. It's also far smaller and themable as text.
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
- **Raster exports re-bake the SVG, they don't re-record.** One source of truth;
  `demo.mp4`/`demo.gif` can't drift from the README animation, and refreshing
  them is a ~50 s Chrome pass with no tmux/transfer involved.
- **Headless Chrome, not a Python SVG renderer, for rasterizing.** Chrome renders
  the CSS + woff2 exactly as a browser does (pixel-identical); `cairosvg`
  mis-rendered the braille spinner glyph as tofu and was ~100× slower (it
  re-parses the embedded font on every frame).

---

## Preview without recording

Watch the choreography live in your own terminal (no file produced):

```bash
scripts/preview/demo.sh
```

`Ctrl-b d` to detach, or just close the terminal — the throwaway server and tmux
session are cleaned up on exit.

---

## Tuning knobs

| want to change | where |
|----------------|-------|
| Pacing / the share-code handoff | `director()` in `demo.sh` (the `sleep`s) |
| Pane labels, colours, layout | `do_setup()` in `demo.sh` |
| Terminal size (columns x rows) | `FSEND_DEMO_COLS` / `FSEND_DEMO_ROWS` (88x33; read by `demo.sh`, `ptyrec.py` and `svg-postprocess.py`) |
| svg-term's column pitch (if svg-term bumps versions) | `CELL` in `svg-postprocess.py` (asserted against the runs) |
| Terminal base colours, title bar, cursor | `THEMES` in `svg-postprocess.py` |
| Dark → light colour mapping | `TOKENS` in `svg-postprocess.py` (mirror `internal/uxlog/glyphs.go`) |
| Shell prompt / card host colour | `PROMPT_RGB`, `HOST_RGB` in `demo.sh` (add the light sibling to `TOKENS`) |
| Loop fade | `FADE_IN_S`, `FADE_OUT_S` in `svg-postprocess.py` |
| End-of-loop pause | `HOLD_S` in `svg-postprocess.py` |
| Coral border / window radius | `MARGIN`, `WIN_R` in `svg-postprocess.py` |
| Transfer size shown in the demo | `PAYLOAD_BYTES` in `demo.sh` (drives the payload + the on-screen size) |
| Traffic-light size / position | `DOT_R`, `DOTS`, `DOT_CX0`/`DOT_GAP` in `svg-postprocess.py` |
| Embedded font | `find_font()` in `svg-postprocess.py` |
| mp4 frame rate / scrub cadence | `FPS` in `render-video.py` |
| Supersample factor (sharpness vs size) | `SCALE` in `render-video.py` |
| gif frame rate / resolution (smoothness vs size) | `GIF_FPS`, `GIF_SCALE` in `render-video.py` |
| Chrome location | `CHROME` in `render-video.py` |

---

## Troubleshooting

- **`record-svg.sh` exits "missing required tool".** Install the dependency it
  names (see TL;DR). `svg-term` is an npm global; `fonttools`/`brotli` are pip.
- **`svg-postprocess.py`: "cast uses #xxxxxx, which has no entry in TOKENS".**
  fsend (or `demo.sh`) now paints a colour the light theme can't map. Add the
  dark hex and its light sibling to `TOKENS` — for a product colour, the pair
  from `internal/uxlog/glyphs.go`.
- **The demo reads slow ("8 MB/s", a long progress phase).** Something is
  throttling the machine's own LAN traffic — typically a VPN split-tunnel or
  transparent proxy. Pause it and re-record (see the TL;DR note).
- **`svg-postprocess.py` raises an `AssertionError`.** svg-term's output format
  changed (new version) and a frame/window/keyframe pattern no longer matches.
  Pin svg-term, or update the matching assertion in `svg-postprocess.py`.
- **The font looks like plain monospace on GitHub.** The embedded data-URI font
  didn't load — confirm `@font-face` is present in `docs/assets/demo.svg` and
  that it's a `data:` URL (not external).
- **Headers scroll off / a pane is clipped.** A pane is shorter than its content;
  check the pane sizing in `director()` (`sc`, `mid`) against `FSEND_DEMO_ROWS`.
- **Transfer shows a LAN-discovery notice or the wrong path.** Confirm the
  `fsend` shim (with `--mode=direct`) is on the panes' `PATH`.
- **`render-video.py`: `ModuleNotFoundError: websocket`.** Install the CDP client:
  `pip install websocket-client` (note: *not* the `websockets` package).
- **`render-video.py`: "Chrome did not expose a DevTools port".** Chrome failed
  to launch — confirm it's installed and that `CHROME` points at the binary.
- **`render-video.py`: "could not hook the SVG animation".** svg-term's output
  changed so the animated `<g style="animation-name:…">` element or its
  `@keyframes` block no longer matches. The script reads whatever animation name
  `svg-term` emitted, so update the selector/parse near the top of
  `render-video.py`.
- **mp4/gif look blurry or jittery.** Both are guarded by design (2× supersample
  + constant-cadence scrub). If it regresses, check `SCALE` is ≥2 and that no
  downscale crept back into the gif filter (`gif_vf`).
