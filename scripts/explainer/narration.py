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
GAP_SAME = 0.70            # pause between two lines in the same scene
GAP_SCENE = 2.40           # pause when the scene changes. This is the single
                           # most important number in the film: it is the time
                           # the viewer gets to *finish reading* one screen
                           # before the next one arrives. Short gaps read as
                           # "too fast to retain".
TAIL = 3.0                 # hold after the last line (settled end card)
# extra hold before a line (on top of the normal gap), to let a screen land
GAP_EXTRA = {
    "cmd2": 0.85,          # the receiver appears after the code has been read
    "modes2": 0.25,        # the three route cards are already on screen
    "local2": 0.35,        # let the LAN link finish drawing
    "direct2": 0.40,       # let the pairing handshake finish
    "relay2": 0.65,        # let the failed direct attempt land
    "cta2": 0.45,          # let the server card land before explaining it
    "cta3": 0.35,          # the two roles stay up long enough to be read
    "outro1": 0.80,        # let the self-host links settle before the end card
    "outro2": -0.60,       # keep the closing line close to the name
}

# How long before its first line a scene should appear (seconds). The default
# is half the scene-change gap; the end card is the exception: the wordmark has
# to be on screen when the voice says the name.
SCENE_LEAD = {"outro": 0.60}

# ---- the script ------------------------------------------------------------
# id, scene, voice-over, anchors {label: spoken substring} — anchors mark when a
# word is actually spoken so the visuals can reveal in sync. Keep lines short and
# warm; this is a promo, not a manual.
LINES = [
    ("brand1", "brand", "Presenting fsend.",
     {}),
    ("brand2", "brand", "Send any file to any device, straight from your terminal.",
     {"anyfile": "any file"}),
    ("cmd1", "cmd", "Point it at a file, and fsend hands you a short code.",
     {"code": "code"}),
    ("cmd2", "cmd", "Your friend runs that code — and the file lands on their machine.",
     {"runs": "runs", "lands": "lands"}),
    # the overview: name the three routes, one short line each, so the slide has
    # something to say for every card it lights up. Each line has to say *why*
    # that route exists, not just its name — this is the only place the film
    # explains all three at once.
    ("modes1", "modes", "Behind the scenes, fsend picks the best path on its own.",
     {}),
    ("modes2", "modes", "Local, when both devices share a network — nothing in between.",
     {"local": "Local"}),
    ("modes3", "modes", "Direct, across the internet — the server just introduces them.",
     {"direct": "Direct"}),
    ("modes4", "modes", "Relayed, when a strict network blocks that — the server carries the file.",
     {"relay": "Relayed"}),
    # the detail slides: one sentence per beat, so nothing has to be rushed
    ("local1", "local", "On the same network, it connects straight to the other device.",
     {"connects": "connects"}),
    ("local2", "local", "The server is not used.",
     {"noserver": "not used"}),
    ("direct1", "direct", "Across the internet, it connects directly — peer to peer.",
     {"connects": "connects"}),
    ("direct2", "direct", "The server only introduces them, then steps aside.",
     {"introduces": "introduces", "steps": "steps"}),
    ("relay1", "relay", "If a strict network blocks a direct connection, fsend relays the file.",
     {"blocked": "blocks", "relays": "relays"}),
    ("relay2", "relay", "The relay only ever sees sealed, encrypted packets.",
     {"sees": "only ever", "sealed": "sealed"}),
    # the server slide: free to use, what it actually does, then how to own one
    ("cta1", "cta", "The public server is free, and needs no setup.",
     {"free": "free"}),
    ("cta2", "cta", "Its only jobs: pairing two devices, and relaying the file when direct is blocked.",
     {"pairing": "pairing", "relaying": "relaying"}),
    ("cta3", "cta", "Want full control? Run your own with a single command.",
     {"control": "control", "command": "command"}),
    ("outro1", "outro", "fsend.",
     {}),
    ("outro2", "outro", "Install it with one command. It runs on Linux, macOS, Windows, and FreeBSD.",
     {"install": "one command", "runs": "runs", "linux": "Linux",
      "windows": "Windows"}),
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
        if i == 0:
            start = 0.0
        elif sc["name"] in SCENE_LEAD:
            start = sc["first"] - SCENE_LEAD[sc["name"]]
        else:
            start = (order[i - 1]["last_end"] + sc["first"]) / 2
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
