# fsend explainer

The fsend promo video: about 65 seconds, 1920×1080 at 60 fps, with a
synthesized voice-over, music and sound effects.

Each frame is a web page (HTML, CSS, JS) rendered by headless Chromium
at a given timestamp. So the same input always gives the same frame,
frames render at 2× for sharp text, and the visuals follow the measured
narration.

This folder holds only the source. Everything generated is git-ignored.

## Design rules

- **One idea per shot**, in big type, matching the narration.
- **Show the real product.** The terminals print exactly what fsend
  prints, and the numbers match the clock.
- **Works muted.** Every scene makes its point in text.
- **One stage for the three routes:** local, direct, relay.
- **Every sound has a cause on screen.**
- **End on the name and the install line.**

## Setup

Needs Python 3.10+, ffmpeg, and internet the first time.

```sh
cd scripts/explainer
./setup.sh --models     # venv, dependencies, Chromium, voice model (~340 MB)
```

Everything installs inside this folder.

## Build

```sh
./make.sh
./make.sh --output ~/Desktop/fsend-explainer.mp4
```

Output: `fsend-explainer.mp4`, and captions in
`captions/fsend-explainer.srt`.

The build runs seven stages: narration (Kokoro voice, timed with
faster-whisper), sound cues, frame capture, audio mix, encode, captions,
mux.

| Flag | What it does |
|---|---|
| `--skip-capture` | Reuses the frames on disk. |
| `--skip-narration` | Reuses the audio and `timeline.json`. |
| `--workers 4 --crf 16 --grain 0` | Capture workers and encode settings. |
| `--music-level -23 --sfx-gain 0.6` | Mix levels. |
| `FPS=30 ./make.sh` | A faster draft. |

Stills while editing, and the README thumbnail:

```sh
.venv/bin/python capture.py --preview 3.6,24.6,41.2   # -> preview/*.png
.venv/bin/python capture.py --poster 6.36             # -> docs/assets/video-poster.png
```

## Files

| File | Role |
|---|---|
| `narration.py` | The script and pacing. **Start here** to change the words. |
| `web/scenes.js` | The seven scenes. |
| `web/engine.js` | The renderer: layers, cameras, cuts, colors, sound cues. |
| `web/style.css` | Colors, type, terminal look. |
| `web/fonts/` | Inter and JetBrains Mono, committed for offline builds. |
| `capture.py` | Renders the frames. |
| `music.py` | The score. |
| `sfx.py` | The sound effects. |
| `build.py` | Runs all stages. |
| `setup.sh`, `make.sh` | Setup and build. |

## Changing things

- **Words and pacing:** `LINES` in `narration.py`. `GAP_SAME`,
  `GAP_SCENE` and `GAP_EXTRA` set the speed. To make a visual land on a
  word, add an anchor and read it with `api.on("<line-id>", "<anchor>")`.
- **Voice:** `KOKORO_VOICE`, `KOKORO_SPEED`.
- **Visuals:** `web/scenes.js` and `web/style.css`. Use
  `camera(cam, t, keys)`, `api.accent(t, hex)` and
  `api.cue(type, t, gain)`.
- **Music:** `BPM` and `CHORDS` in `music.py`. The beat lines up with the
  logo hit.

## Pitfalls

- Use radial gradients for soft light, not `filter: blur()` on big
  layers: it's slow in the software renderer.
- Hide scene layers with `display: none`, not `visibility: hidden`.
- Apply gradient text per word, not on the parent.
