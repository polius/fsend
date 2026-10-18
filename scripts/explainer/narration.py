#!/usr/bin/env python3
"""Narration: the single source of truth for the explainer's words and timing.

Audio-first, exactly like the old pipeline: every line is synthesized and
measured, then a timeline is laid out so the visuals can track the voice. On top
of that we export *word onsets* — for each line you can tag a spoken substring
(an "anchor") and get the moment it is said; the web scenes use those to reveal
things exactly in step with the narration.

Outputs (all git-ignored):
  audio/<id>.wav      one clip per line (Kokoro neural TTS)
  narration.wav       the stitched voice track (48 kHz stereo)
  timeline.json       segments + scene windows + onsets, consumed by the renderer

Usage:
  python narration.py              # synthesize (Kokoro) then lay out the timeline
  python narration.py --reuse      # reuse audio/*.wav, just re-lay out the timeline
"""
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
AUD = os.path.join(HERE, "audio")

# ---- voice -----------------------------------------------------------------
MODEL = os.environ.get("KOKORO_MODEL", os.path.join(HERE, "models", "kokoro-v1.0.onnx"))
VOICES = os.environ.get("KOKORO_VOICES", os.path.join(HERE, "models", "voices-v1.0.bin"))
VOICE = os.environ.get("KOKORO_VOICE", "af_heart")   # warm, confident
SPEED = float(os.environ.get("KOKORO_SPEED", "0.97"))
LANG = "en-us"

# ---- pacing ----------------------------------------------------------------
FPS = 30
LEAD_IN = 0.9              # silence before the first line
GAP_SAME = 0.42            # pause between two lines in the same scene
GAP_SCENE = 0.85           # pause when the scene changes (breathing room)
TAIL = 6.0                 # hold after the last line (long, settled outro)
# extra hold before a line (on top of the normal gap), to let a screen land
GAP_EXTRA = {"cmd2": 0.9, "modes1": 0.7}

# ---- the script ------------------------------------------------------------
# id, scene, voice-over, anchors {label: spoken substring} — anchors mark when a
# word is actually spoken so the visuals can reveal in sync. Keep lines short and
# warm; this is a promo, not a manual.
LINES = [
    ("brand1", "brand", "Presenting fsend.",
     {}),
    ("brand2", "brand", "Send any file, straight from one computer to another.",
     {"anyfile": "any file"}),
    ("cmd1", "cmd", "Point it at a file, and fsend hands you a short code.",
     {"code": "code"}),
    ("cmd2", "cmd", "Your friend runs that code — and the file lands on their machine.",
     {"runs": "runs", "lands": "lands"}),
    ("modes1", "modes", "Behind the scenes, fsend picks the best path on its own — local, direct, or relayed.",
     {"local": "local", "direct": "direct", "relay": "relayed"}),
    ("local1", "local", "On the same network, it connects straight to the other device — your file never touches a server.",
     {"connects": "connects", "noserver": "never touches"}),
    ("direct1", "direct", "Across the internet, it connects directly — peer to peer. The server only introduces them, then steps aside.",
     {"connects": "connects", "introduces": "introduces", "steps": "steps"}),
    ("relay1", "relay", "And if a strict network blocks a direct path, it falls back to a relay, which only ever sees sealed, encrypted packets.",
     {"blocked": "blocks", "relay": "relay", "sealed": "sealed"}),
    ("cta1", "cta", "The public server is free, and needs no setup.",
     {"free": "free"}),
    ("cta2", "cta", "Want full control? Run your own with a single command.",
     {"control": "control", "command": "command"}),
    ("outro1", "outro", "fsend.",
     {}),
    ("outro2", "outro", "Simple, private, free.",
     {"simple": "Simple", "private": "private", "free": "free"}),
]


def run(cmd):
    subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)


def duration(path):
    out = subprocess.check_output([
        "ffprobe", "-v", "error", "-show_entries", "format=duration",
        "-of", "default=noprint_wrappers=1:nokey=1", path])
    return float(out.strip())


def synth():
    """Synthesize every line with the local Kokoro neural voice -> audio/<id>.wav."""
    os.makedirs(AUD, exist_ok=True)
    if not os.path.exists(MODEL) or not os.path.exists(VOICES):
        sys.exit(f"missing Kokoro models:\n  {MODEL}\n  {VOICES}\n"
                 "run ./setup.sh (or set KOKORO_MODEL / KOKORO_VOICES)")
    import soundfile as sf
    from kokoro_onnx import Kokoro
    kokoro = Kokoro(MODEL, VOICES)
    for sid, _scene, vo, _anchors in LINES:
        path = os.path.join(AUD, f"{sid}.wav")
        samples, sr = kokoro.create(vo, voice=VOICE, speed=SPEED, lang=LANG)
        sf.write(path, samples, sr)
        print(f"  synth {sid:8s} {len(samples) / sr:5.2f}s  {vo}")


def make_silence(sec):
    sec = round(sec, 3)
    path = os.path.join(AUD, f"sil_{int(sec * 1000)}.wav")
    if not os.path.exists(path):
        run(["ffmpeg", "-y", "-f", "lavfi", "-t", f"{sec}",
             "-i", "anullsrc=r=24000:cl=mono", path])
    return path


def onset(vo, dur, start, sub):
    """Time (s) at which `sub` is spoken, estimated from its character position.

    Speech pace is close enough to uniform within a short sentence; using the
    character ratio keeps onsets adaptive to whatever the TTS engine's timing is.
    """
    i = vo.find(sub)
    if i < 0:
        raise ValueError(f"anchor {sub!r} not found in {vo!r}")
    return round(start + dur * (i / max(1, len(vo))), 3)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--reuse", action="store_true",
                    help="reuse audio/*.wav instead of re-synthesizing")
    args = ap.parse_args()

    if not args.reuse:
        print("synthesizing narration (Kokoro)")
        synth()

    durs = [duration(os.path.join(AUD, f"{sid}.wav")) for sid, *_ in LINES]

    # ---- lay out the timeline from real audio durations --------------------
    segs, parts, t = [], [make_silence(LEAD_IN)], LEAD_IN
    for i, (sid, scene, vo, anchors) in enumerate(LINES):
        if i > 0:
            gap = GAP_SCENE if scene != LINES[i - 1][1] else GAP_SAME
            gap += GAP_EXTRA.get(sid, 0.0)
            parts.append(make_silence(gap))
            t += gap
        start, dur = t, durs[i]
        parts.append(os.path.join(AUD, f"{sid}.wav"))
        t += dur
        segs.append({
            "id": sid, "scene": scene, "vo": vo,
            "start": round(start, 3), "dur": round(dur, 3),
            "end": round(start + dur, 3),
            "onsets": {k: onset(vo, dur, start, s) for k, s in anchors.items()},
        })
    parts.append(make_silence(TAIL))
    video_end = round(t + TAIL, 3)

    # ---- stitch the voice track -------------------------------------------
    listfile = os.path.join(AUD, "concat.txt")
    with open(listfile, "w") as f:
        for p in parts:
            f.write(f"file '{p}'\n")
    narration = os.path.join(HERE, "narration.wav")
    run(["ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", listfile,
         "-ar", "48000", "-ac", "2", narration])

    # ---- scene windows (boundaries centred in the scene-change gaps) -------
    order = []
    for s in segs:
        if not order or order[-1]["name"] != s["scene"]:
            order.append({"name": s["scene"], "first": s["start"], "last_end": s["end"]})
        else:
            order[-1]["last_end"] = s["end"]
    scenes = []
    for i, sc in enumerate(order):
        start = 0.0 if i == 0 else (order[i - 1]["last_end"] + sc["first"]) / 2
        scenes.append({"name": sc["name"], "start": round(start, 3)})
    for i in range(len(scenes)):
        scenes[i]["end"] = round(scenes[i + 1]["start"], 3) if i + 1 < len(scenes) else video_end

    timeline = {"fps": FPS, "video_end": video_end,
                "narration_dur": round(duration(narration), 3),
                "segments": segs, "scenes": scenes}
    with open(os.path.join(HERE, "timeline.json"), "w") as f:
        json.dump(timeline, f, indent=2)

    print(f"\nnarration {timeline['narration_dur']:.2f}s   video {video_end:.2f}s "
          f"({video_end / 60:.2f} min)")
    for sc in scenes:
        print(f"  {sc['name']:7s} {sc['start']:6.2f} -> {sc['end']:6.2f}")


if __name__ == "__main__":
    main()
