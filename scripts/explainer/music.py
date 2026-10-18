#!/usr/bin/env python3
"""Generate the ambient music bed: a slow, consonant pad, deliberately quiet.

It is synthesized from scratch (no samples, no licensing) so the video never
depends on a music asset. Kept very low in the mix — it is there to warm the
silence under the narration and carry continuity across scene changes, not to be
noticed.
"""
from __future__ import annotations

import argparse
import os

import numpy as np
import soundfile as sf

HERE = os.path.dirname(os.path.abspath(__file__))
SR = 48000

# A slow, resolved progression: C – Am – F – G – C
CHORDS = {
    "C":  [130.81, 164.81, 196.00, 261.63],
    "Am": [110.00, 130.81, 164.81, 220.00],
    "F":  [87.31, 110.00, 130.81, 174.61],
    "G":  [98.00, 123.47, 146.83, 196.00],
}
PROGRESSION = [("C", 0), ("Am", 14), ("F", 28), ("G", 42), ("C", 52)]
FADE = 4.0            # crossfade between chords (s)
LEVEL = 0.030         # peak, ~-30 dBFS — a bed, not a track
PARTIALS = [(1, 1.00), (2, 0.30), (3, 0.11), (4, 0.045), (5, 0.02)]


def smoothstep(x):
    x = np.clip(x, 0.0, 1.0)
    return x * x * (3 - 2 * x)


def lp(x, cutoff):
    """Zero-phase 1st-order low-pass via the frequency domain (offline, so we
    can afford the FFT and avoid a sample-by-sample loop)."""
    X = np.fft.rfft(x)
    f = np.fft.rfftfreq(x.size, 1.0 / SR)
    X *= 1.0 / np.sqrt(1.0 + (f / cutoff) ** 2)
    return np.fft.irfft(X, x.size)


def build(duration):
    n = int(duration * SR)
    t = np.arange(n) / SR
    rng = np.random.default_rng(7)
    out = np.zeros(n)

    starts = [s for _, s in PROGRESSION]
    for i, (name, a) in enumerate(PROGRESSION):
        b = starts[i + 1] if i + 1 < len(starts) else duration
        # window: rise over [a, a+FADE], fall over [b, b+FADE]
        rise = smoothstep((t - a) / FADE)
        fall = 1.0 - smoothstep((t - b) / FADE) if i + 1 < len(starts) else np.ones(n)
        window = rise * fall
        if not window.any():
            continue
        tones = CHORDS[name]
        voice = np.zeros(n)
        for j, f in enumerate(tones):
            amp = 1.0 / (1 + 0.5 * j)
            for k, p in PARTIALS:
                det = 1.0 + (rng.random() - 0.5) * 0.0024
                ph = rng.random() * 2 * np.pi
                voice += amp * p * np.sin(2 * np.pi * f * k * det * t + ph)
        # a soft sub under the lowest note
        voice += 0.22 * np.sin(2 * np.pi * tones[0] / 2 * t)
        out += window * voice

    out /= max(1e-9, np.max(np.abs(out)))
    out = lp(out, 950)
    out = lp(out, 950)
    # slow amplitude breathing
    out *= 1.0 + 0.07 * np.sin(2 * np.pi * 0.06 * t + 0.7)
    # global fade in/out
    out *= smoothstep(t / 2.0)
    out *= 1.0 - smoothstep((t - (duration - 3.0)) / 3.0)
    out *= LEVEL
    # gentle stereo width (Haas)
    delay = int(0.009 * SR)
    left = out
    right = np.concatenate([np.zeros(delay), out[:-delay]])
    stereo = np.stack([left, right], axis=1)
    peak = float(np.max(np.abs(stereo)))
    if peak > 1.0:
        stereo /= peak
    return stereo


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--duration", type=float, required=True)
    ap.add_argument("--out", default=os.path.join(HERE, "music.wav"))
    args = ap.parse_args()
    data = build(args.duration)
    sf.write(args.out, data, SR, subtype="PCM_16")
    print(f"music: {args.out}  {args.duration:.1f}s  peak {np.max(np.abs(data)):.4f}")


if __name__ == "__main__":
    main()
