# fsend explainer

The fsend explainer/promo — a **60-second, 1920×1080 MP4** with a neural
voice-over, a synthesized ambient bed, and a soft Apple-keynote look.

It walks through the value prop and the **three transfer modes**
(local / direct / relay) in the same order as the previous pipeline, but it is
built from scratch: a small **web "video engine"** (HTML/CSS/JS) rendered
deterministically by headless Chromium, one frame at a time.

This folder holds **only the source**. Frames, audio, the model weights and the
MP4 itself are all generated and git-ignored — regenerate with `./make.sh`.

---

## Why not a normal video editor / screen recording

It is the same trick as a chart library: if every pixel is a **pure function of
the timestamp**, then

- the same input always produces the same frame (reviews are meaningful),
- we can render at 2× and downsample, so type and thin strokes stay crisp at
  1080p (supersampling — the main reason it looks sharper than a screen grab),
- timing is driven by the *measured* narration audio, so the visuals stay in
  sync no matter how fast the voice reads.

So the "renderer" is a web page, and the "render farm" is
`page.screenshot()` in a loop.

---

## What it produces

| Output | Notes |
|---|---|
| `fsend-explainer.mp4` | the deliverable — 1080p30, H.264 + AAC, voice at −16 LUFS |
| `captions/fsend-explainer.srt` | captions in a **subfolder**, so no player auto-attaches them: off by default, available to load when wanted |

Scene arc, timed to the narration:

```
brand  →  one command to send / to receive  →  how fsend works (3 modes)
       →  local  →  direct  →  relay  →  self-host CTA  →  outro
```

---

## Setup

Needs **Python 3.10+**, **ffmpeg**, and a network connection for the first run.

```sh
cd scripts/explainer
./setup.sh --models        # venv + deps + chromium + Kokoro weights (~340 MB)
```

Everything is installed *inside this folder* (`.venv/`, `models/`) — nothing is
installed system-wide, and the repo is untouched.

---

## Build

```sh
./make.sh                  # the whole pipeline
```

Stages (`build.py` prints them as `[n/6]`):

1. **narration** — `narration.py` synthesizes every line with **Kokoro-82M**
   (local ONNX, voice `af_heart`), measures the clips and lays out
   `timeline.json`: scene windows plus *word onsets*, so a scene can reveal
   something on the exact word it is spoken.
2. **capture** — `capture.py` serves `web/` and drives headless Chromium,
   calling `window.__render(t)` and screenshotting each of the ~1,800 frames at
   **2×** (3840×2160). Runs across several processes in parallel.
3. **audio** — normalizes the voice to −16 LUFS and mixes in the ambient bed
   from `music.py` (synthesized C–Am–F–G pad, no samples to license).
4. **encode** — lanczos-downscale to 1080p, a whisper of film grain, x264 CRF 18.
5. **captions** — a `.srt` sidecar, one cue per sentence.
6. **mux** — video + mix → `fsend-explainer.mp4`.

Useful flags:

```sh
./make.sh --skip-capture          # re-encode from frames/ already on disk
./make.sh --skip-narration        # reuse audio/ + timeline.json
./make.sh --output ~/Desktop/fsend-explainer.mp4
./make.sh --workers 4 --crf 16 --grain 0
```

To look at a few stills while iterating on the visuals (fast — 1×, no encode):

```sh
.venv/bin/python capture.py --preview 3.6,24.6,41.2      # -> preview/*.png
```

To regenerate the README thumbnail (a still with a play button, downscaled and
grained exactly like the encode):

```sh
.venv/bin/python capture.py --poster 5.3                # -> docs/assets/video-poster.png
```

---

## Files

| File | Role |
|---|---|
| `narration.py` | the script + timing. **Start here** to change the wording. |
| `timeline.json` | generated: segments, scene windows, word onsets |
| `web/index.html` | page shell; the only SVG defs (no blur filters — see below) |
| `web/style.css` | palette, type, cards, the wordmark |
| `web/engine.js` | the renderer: fixed-timestep `render(t)`, scene layers, dip transitions, background, timing helpers |
| `web/scenes.js` | the eight scenes and everything that animates inside them |
| `web/fonts/` | Inter + JetBrains Mono (both OFL), committed for offline builds |
| `capture.py` | http server + Playwright frame loop (`--preview` for stills) |
| `music.py` | the ambient bed |
| `build.py` | orchestrates all six stages |
| `setup.sh` / `make.sh` | one-time setup / one-shot build |

---

## Customizing

- **Wording & pacing** — edit `LINES` in `narration.py`, then `./make.sh`.
  Adding a line? Give it the scene it belongs to and, if a visual should land on
  a particular word, an anchor (`{"sealed": "sealed"}`); scenes read those with
  `api.on("<line-id>", "<anchor>")`. Everything re-syncs automatically, because
  the timeline is derived from the freshly synthesized audio.
- **Voice / speed** — `KOKORO_VOICE` / `KOKORO_SPEED` env vars (`narration.py`
  defaults to `af_heart` at `0.97`). Other warm female voices: `af_bella`,
  `af_nicole`, `af_sarah`; male: `am_michael`, `am_fenrir`.
- **Visuals** — `web/scenes.js` (what happens) and `web/style.css` (how it
  looks). Iterate with `capture.py --preview`; only re-run capture once happy.
- **Speed / quality** — `--scale 1` for a fast draft, `--crf`/`--grain`/`--workers`
  to tune the encode.

---

## Notes

- **Rendering cost is fill-rate bound.** The two things that dominate a frame
  are live blur filters (`filter: blur()`, SVG `feGaussianBlur`) and large
  translucent layers, because the software rasterizer re-runs them for every
  screenshot. Soft shapes are therefore drawn with *radial gradients* — see the
  comment in `web/index.html` — and the film grain is applied once by ffmpeg at
  the end instead of being a per-frame CSS layer. Parallel capture
  (`--workers`) covers the rest.
- **Do not commit** `frames/`, `preview/`, `audio/`, `captions/`,
  `timeline.json`, `*.wav`, `*.mp4`, `models/` or `.venv/` — all git-ignored.
- The `scripts/video/` pipeline (Pillow-based) is the previous implementation and
  is kept for reference only.
