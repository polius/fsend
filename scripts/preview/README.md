# README demo

Generates the animated demo at the top of the README:

- `docs/assets/demo.svg` (dark) and `docs/assets/demo-light.svg`.
- `docs/assets/demo.mp4` and `demo.gif`, for places that don't show SVG.

It records a real fsend transfer between two terminals, fully offline:
a loopback server, a throwaway config, random data.

## Regenerate

```bash
# one-time setup
brew install tmux
brew install --cask font-jetbrains-mono
npm install -g svg-term-cli
pip install fonttools brotli websocket-client

# the two SVGs
scripts/preview/record-svg.sh

# the mp4 and gif, from demo.svg (needs Chrome and ffmpeg)
python3 scripts/preview/render-video.py
```

`record-svg.sh` builds fsend from this checkout, records once, and
renders both themes. Re-run it whenever the CLI output changes, then
`render-video.py`.

**Check the speed on screen before committing.** A VPN or network filter
can slow the transfer to a few MB/s, which makes fsend look slow. Pause
it and record again.

To watch the demo live without recording: `scripts/preview/demo.sh`.

## Files

| File | Role |
|---|---|
| `demo.sh` | Sets up the two tmux panes and types the transfer. |
| `ptyrec.py` | Records the session as an asciinema cast at 88x33. |
| `svg-postprocess.py` | Themes the cast, renders it with svg-term, and adds the cursor, final hold, fade, window frame and embedded font. |
| `record-svg.sh` | Runs it all: build, record, render both SVGs. |
| `render-video.py` | Turns `demo.svg` into the mp4 and gif with headless Chrome. |

## Why it's built this way

- **SVG in the README:** sharp at any size, small, and cheap to play.
- **The font is embedded:** GitHub blocks external fonts in SVG images.
- **CSS animation only:** GitHub strips scripts from SVG.
- **`ptyrec.py`:** asciinema can't fix the terminal size headlessly, and
  termtosvg can't record full-screen tmux.
- **The mp4 and gif come from the SVG**, so they always match it.
- **Chrome renders the frames:** other SVG renderers got the font wrong.

## Settings

| To change | Where |
|---|---|
| Pacing | `director()` in `demo.sh` |
| Pane labels and layout | `do_setup()` in `demo.sh` |
| Terminal size | `FSEND_DEMO_COLS`, `FSEND_DEMO_ROWS` (88x33) |
| Transfer size | `PAYLOAD_BYTES` in `demo.sh` |
| Theme colors, title bar | `THEMES` in `svg-postprocess.py` |
| Dark → light color map | `TOKENS` in `svg-postprocess.py` (mirror `internal/uxlog/glyphs.go`) |
| Final pause, fade | `HOLD_S`, `FADE_IN_S`, `FADE_OUT_S` in `svg-postprocess.py` |
| Frame and window corners | `MARGIN`, `WIN_R` in `svg-postprocess.py` |
| Video frame rate, sharpness | `FPS`, `SCALE` in `render-video.py` |
| GIF frame rate and size | `GIF_FPS`, `GIF_SCALE` in `render-video.py` |
| Chrome path | `CHROME` in `render-video.py` |

## Troubleshooting

| Problem | Fix |
|---|---|
| "missing required tool" | Install what it names (see setup). |
| "cast uses #xxxxxx, which has no entry in TOKENS" | Add that color and its light pair to `TOKENS`. |
| The demo shows a slow transfer | Pause your VPN or network filter and record again. |
| `AssertionError` in `svg-postprocess.py` | svg-term's output changed. Pin svg-term or update the assertion. |
| Plain font on GitHub | Check `demo.svg` has an `@font-face` with a `data:` URL. |
| A pane is cut off | Check the pane sizes in `director()` against `FSEND_DEMO_ROWS`. |
| `ModuleNotFoundError: websocket` | `pip install websocket-client` (not `websockets`). |
| "Chrome did not expose a DevTools port" | Check Chrome is installed and `CHROME` points at it. |
| "could not hook the SVG animation" | svg-term's output changed. Update the parsing at the top of `render-video.py`. |
