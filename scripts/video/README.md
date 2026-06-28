# fsend explainer video

Generates the ~96-second animated explainer/promo for fsend — a 1920×1080 MP4
with **neural voice-over**, on-screen captions, and the fsend terminal aesthetic
(JetBrains Mono, terracotta `#E0805A`). It walks through the value prop and the
**three transfer modes** (local / direct / relay).

This folder contains **only the source**. The MP4, audio, frames, model weights
and venv are all generated (and git-ignored) — regenerate with `./make.sh`.

> Note: this is the marketing/explainer video. The terminal-usage screen
> recording lives in [`../preview`](../preview) — different tool, different output.

---

## What it produces

- `fsend-explainer.mp4` — the deliverable: neural narration (no embedded subtitles)
- `captions/fsend-explainer.srt` — captions, written to a **subfolder** so no player
  auto-attaches them: **off by default**, available to load manually or upload as a
  toggleable caption track
- 1920×1080, 30 fps, H.264 + AAC, voice normalized to −16 LUFS

Scene arc (timed to the narration): problem hook → brand → one-command demo →
**local / direct / relay** → self-host CTA → outro.

---

## Prerequisites (one-time, macOS)

1. **Python 3**, **ffmpeg**, **Homebrew**.
2. **JetBrains Mono** font installed in `~/Library/Fonts` (the renderer reads it
   from there; on Linux, edit `FONT_DIR` in `render.py`). Download:
   https://www.jetbrains.com/lp/mono/
3. **espeak-ng** (phonemizer backend for the neural TTS):
   ```sh
   brew install espeak-ng
   ```
   The pip-bundled espeak loader ships a broken compiled-in data path, so we point
   phonemizer at the Homebrew copy (`/opt/homebrew/lib/libespeak-ng.dylib`,
   `/opt/homebrew/share/espeak-ng-data`). `synth_kokoro.py` sets this automatically.
4. **Kokoro neural TTS** in an isolated venv, plus its model weights:
   ```sh
   cd scripts/video
   python3 -m venv .venv-tts
   .venv-tts/bin/pip install -U pip
   .venv-tts/bin/pip install kokoro-onnx soundfile numpy

   mkdir -p models && cd models
   curl -L -O https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx   # ~310 MB
   curl -L -O https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin    # ~27 MB
   cd ..
   ```

---

## Generate

```sh
cd scripts/video
./make.sh
```

`make.sh` runs the whole pipeline:

1. **synth** — `synth_kokoro.py` → `audio/<id>.wav` (local Kokoro neural voice)
2. `REUSE_WAVS=1 python3 build_audio.py` — measure the clips, lay out the
   audio-driven timeline, assemble `narration.wav` + `timeline.json`
3. `python3 render.py` — render all frames → `frames/` (reads `timeline.json`)
4. normalize the voice to −16 LUFS, build a sentence-timed `.srt`, encode video
5. mux → `fsend-explainer.mp4` (video + voice). The `.srt` ships **alongside** as a
   sidecar so captions are **off by default** (ffmpeg's mp4 muxer always force-marks
   an embedded subtitle track as on, which we don't want).

Render is the slow step (~2–3 min for ~2,900 frames). Total run a few minutes.

To run a single step (e.g. after editing visuals only), see the commands inside
`make.sh` — they can be run individually.

---

## Files

| File | Role |
|------|------|
| `build_audio.py`  | Source of truth for the **narration** (`SEGMENTS`: id, scene, beat, voice line) and pacing (gaps). Builds `narration.wav` + `timeline.json`. With `REUSE_WAVS=1` it reuses `audio/*.wav` instead of re-synthesizing. (Falls back to macOS `say` if run without REUSE_WAVS — non-neural, for quick offline tests only.) |
| `synth_kokoro.py` | Synthesizes each segment with **Kokoro** neural TTS, local (voice/speed here). Imports `SEGMENTS` from `build_audio.py`. |
| `render.py`       | Draws every frame (Pillow). Reads `timeline.json`; all scenes, animation, captions, cross-dissolves live here. Brand palette + `FONT_DIR` at the top. |
| `make.sh`         | One-shot orchestration (synth → timeline → frames → encode → mux). |
| `voiceover.md`    | Human-readable narration script with timecodes + accuracy notes. |

---

## Customizing

- **Wording / pacing**: edit `SEGMENTS` (and `GAP_*` / `TAIL`) in `build_audio.py`,
  then `./make.sh`. Because timing is audio-driven, captions and animation
  re-sync automatically to the new voice durations.
- **Voice / speed**: edit `VOICE` / `SPEED` in `synth_kokoro.py`. Other warm
  female options: `af_bella`, `af_nicole`, `af_sarah`, `af_aoede`; male:
  `am_michael`, `am_fenrir`. Brand name reads correctly as "ef-send".
- **Visuals**: scene functions and the palette/layout constants are in `render.py`.

---

## Notes
- Do **not** commit the generated MP4s, `models/`, `.venv-tts/`, `audio/`,
  `frames/`, `*.wav`, or `timeline.json` — they're in `.gitignore`.
- The renderer is macOS-oriented for fonts only; the rest is portable. On Linux,
  set `FONT_DIR` in `render.py` and use the system `espeak-ng`/`libespeak-ng.so`.
