# fsend explainer

The fsend promo: a **~57-second, 1920×1080, 60 fps MP4** with a neural
voice-over, an original synthesized score, and sound design cued from the
picture.

It is built from scratch: a small **web "video engine"** (HTML/CSS/JS)
rendered deterministically by headless Chromium, one frame at a time.

Design rules the film follows, so any future edit stays on-model:

- **One idea per shot.** Every screen says one thing, in big type, and the
  narration says the same thing at the same moment.
- **Show the product, not slides.** The demo is two real-looking terminals with
  real `fsend` output; the share code physically flies from one to the other,
  and the file follows the same arc.
- **The three routes share one stage.** Your laptop, their laptop, the server.
  The stage morphs between local → direct → relay (the network splits, the
  server introduces and steps aside, a wall blocks and the relay takes over),
  so the viewer only ever learns one new thing.
- **Every sound has a cause on screen.** Cuts whoosh, keys click, the code
  pops, the logo hits. Scenes register their own cues, so sound and picture
  cannot drift apart.
- **The film ends on a promise:** the name, one line to install, everywhere it
  runs.

This folder holds **only the source**. Frames, audio, the model weights and the
MP4 itself are all generated and git-ignored — regenerate with `./make.sh`.

---

## Why not a video editor / screen recording

If every pixel is a **pure function of the timestamp**, then

- the same input always produces the same frame (reviews are meaningful),
- we can render at 2× and downsample, so type and thin strokes stay crisp at
  1080p (supersampling),
- timing is driven by the *measured* narration audio, so the visuals stay in
  sync no matter how fast the voice reads.

So the "renderer" is a web page, and the "render farm" is `page.screenshot()`
in a loop.

---

## What it produces

| Output | Notes |
|---|---|
| `fsend-explainer.mp4` | 1080p60, H.264 (BT.709) + AAC 256k, mix at −14 LUFS / −1 dBTP |
| `captions/fsend-explainer.srt` | captions in a **subfolder**, so no player auto-attaches them |

Scene arc, timed to the narration:

```
open (cursor types the name, logo hit)  →  demo (send / receive)  →  trust
     →  routes (local → direct → relay, one stage)  →  self-host  →  outro
```

---

## Setup

Needs **Python 3.10+**, **ffmpeg**, and a network connection for the first run.

```sh
cd scripts/explainer
./setup.sh --models        # venv + deps + chromium + Kokoro weights (~340 MB)
```

Everything is installed *inside this folder* (`.venv/`, `models/`).

---

## Build

```sh
./make.sh                                          # the whole pipeline
./make.sh --output ~/Desktop/fsend-explainer.mp4
```

Stages (`build.py` prints them as `[n/7]`):

1. **narration** — `narration.py` synthesizes every line with **Kokoro-82M**
   (local ONNX, voice `af_heart`), and lays out `timeline.json`: scene windows
   plus *word onsets*. Each clip is transcribed back with **faster-whisper**
   (`small.en`, fetched once into `models/whisper/`): its word timestamps place
   the onsets, so a visual lands on the exact word it is spoken, and the
   transcript is printed as `heard:` — a mispronounced word shows up in the
   build log. Without faster-whisper, onsets fall back to the pauses detected
   in the audio.
   The name is synthesized from phonemes (`NAME_IPA`): Kokoro's own reading
   stresses both syllables and the second drifts to "sand".
2. **cues** — the page is loaded once and the scenes' sound-effect cues are
   dumped to `cues.json`.
3. **capture** — `capture.py` drives headless Chromium, calling
   `window.__render(t)` and screenshotting every frame at **2×**, across
   parallel workers.
4. **audio** — `sfx.py` renders the cues, `music.py` composes the score, the
   voice ducks the score through a sidechain compressor, and the mix is
   two-pass loudness-normalized.
5. **encode** — lanczos-downscale to 1080p, BT.709 tags, a whisper of grain,
   x264 CRF 17.
6. **captions** — a `.srt` sidecar, one cue per sentence.
7. **mux**.

Useful flags:

```sh
./make.sh --skip-capture          # re-encode / re-mix from frames/ on disk
./make.sh --skip-narration        # reuse audio/ + timeline.json
./make.sh --workers 4 --crf 16 --grain 0
./make.sh --music-level -23 --sfx-gain 0.6
FPS=30 ./make.sh                  # a faster draft (also re-times the narration)
```

To look at stills while iterating on the visuals (fast — 1×, no encode):

```sh
.venv/bin/python capture.py --preview 3.6,24.6,41.2      # -> preview/*.png
```

To regenerate the README thumbnail:

```sh
.venv/bin/python capture.py --poster 5.8                # -> docs/assets/video-poster.png
```

---

## Files

| File | Role |
|---|---|
| `narration.py` | the script + pacing. **Start here** to change the wording. |
| `web/engine.js` | the renderer: `render(t)`, scene layers + cameras, push-through cuts with a light sweep, the background, accent colour keys, the cue registry |
| `web/scenes.js` | the six scenes and everything that animates inside them |
| `web/style.css` | palette, type, terminal, glass cards |
| `web/fonts/` | Inter + JetBrains Mono (both OFL), committed for offline builds |
| `capture.py` | http server + Playwright frame loop (`--preview`, `--poster`, `--cues`) |
| `music.py` | the score: 112 BPM, Am–F–C–G; kick, hats, claps, bass, supersaw pad, pluck arpeggio, risers, sidechain, convolution reverb — arranged from the timeline |
| `sfx.py` | the sound design: whoosh, key, enter, pop, swish, hit, thud, chime, deny, blip, tick, stream |
| `build.py` | orchestrates all seven stages |
| `setup.sh` / `make.sh` | one-time setup / one-shot build |

---

## Customizing

- **Wording & pacing** — edit `LINES` in `narration.py`. `GAP_SAME`,
  `GAP_SCENE` and `GAP_EXTRA` are the only knobs that decide how fast the film
  feels. Adding a line? Give it a scene and, if a visual should land on a word,
  an anchor (`{"sealed": "sealed"}`); scenes read it with
  `api.on("<line-id>", "<anchor>")`.
- **Voice / speed** — `KOKORO_VOICE` / `KOKORO_SPEED` (`af_heart` at `1.02`).
- **Visuals** — `web/scenes.js` and `web/style.css`. A scene moves its camera
  with `camera(cam, t, keys)`, recolours the film with `api.accent(t, hex)`,
  and makes a sound with `api.cue(type, t, gain)`.
- **Music** — `BPM`, `CHORDS` and the section logic in `music.py`. The beat
  grid is anchored on the first logo `hit` cue, so the drop always lands on it.

---

## Notes

- **Rendering cost is fill-rate bound.** Soft light is drawn with radial
  gradients, never `filter: blur()` on large layers (the software rasterizer
  re-runs it every frame); short blurs only appear during cuts and reveals.
  Film grain is added once by ffmpeg.
- Hidden scene layers use `display: none`, not `visibility: hidden`: a child
  set to `visibility: visible` by a reveal would otherwise show through.
- Gradient text in kinetic headlines is applied per word: `background-clip:
  text` on the parent paints through the word masks.
- **Do not commit** `frames/`, `preview/`, `audio/`, `captions/`,
  `timeline.json`, `cues.json`, `*.wav`, `*.mp4`, `models/` or `.venv/`.
