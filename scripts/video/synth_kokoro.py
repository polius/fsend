#!/usr/bin/env python3
"""Synthesize each narration segment with Kokoro (neural TTS), voice af_heart.

Run with the TTS venv:  .venv-tts/bin/python synth_kokoro.py
Writes audio/<id>.wav (48k stereo) for every SEGMENT, ready for build_audio.py
(run that next with REUSE_WAVS=1).
"""
import os
import subprocess
import tempfile

# point phonemizer at the Homebrew espeak-ng (bundled loader has a bad default path)
ESPEAK_LIB = "/opt/homebrew/lib/libespeak-ng.dylib"
os.environ.setdefault("ESPEAK_DATA_PATH", "/opt/homebrew/share/espeak-ng-data")
os.environ.setdefault("PHONEMIZER_ESPEAK_LIBRARY", ESPEAK_LIB)
from phonemizer.backend.espeak.wrapper import EspeakWrapper
EspeakWrapper.set_library(ESPEAK_LIB)

import soundfile as sf
from kokoro_onnx import Kokoro

from build_audio import SEGMENTS, AUD

VOICE = "af_heart"      # warm, confident female (Kokoro flagship)
SPEED = 0.98            # measured promo pace
LANG = "en-us"


def main():
    os.makedirs(AUD, exist_ok=True)
    k = Kokoro(os.path.join("models", "kokoro-v1.0.onnx"),
               os.path.join("models", "voices-v1.0.bin"))
    for (sid, scene, beat, vo) in SEGMENTS:
        samples, sr = k.create(vo, voice=VOICE, speed=SPEED, lang=LANG)
        with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tf:
            tmp = tf.name
        sf.write(tmp, samples, sr)
        out = os.path.join(AUD, f"{sid}.wav")
        subprocess.run(["ffmpeg", "-y", "-i", tmp, "-ar", "48000", "-ac", "2",
                        out], check=True, stdout=subprocess.DEVNULL,
                       stderr=subprocess.DEVNULL)
        os.remove(tmp)
        print(f"  {sid:8s} {len(samples)/sr:5.2f}s")
    print("kokoro synthesis done")


if __name__ == "__main__":
    main()
