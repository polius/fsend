#!/usr/bin/env python3
"""Build the fsend explainer end to end.

  narration (Kokoro)  ->  timeline.json  ->  headless-Chromium frames  ->
  h264 + voice + music ->  fsend-explainer.mp4  (+ captions sidecar)

Run it from anywhere; paths are resolved relative to this file.

  python build.py                      # the whole thing
  python build.py --skip-capture       # re-encode from frames/ already on disk
  python build.py --skip-narration     # reuse audio/ + timeline.json
  python build.py --no-music
  python build.py --output ~/Desktop/fsend-explainer.mp4
"""
from __future__ import annotations

import argparse
import json
import os
import re
import shutil
import subprocess
import sys
import time

HERE = os.path.dirname(os.path.abspath(__file__))
FRAMES = os.path.join(HERE, "frames")
CAPTIONS = os.path.join(HERE, "captions")
PY = sys.executable


def run(cmd, **kw):
    print("  $", " ".join(str(c) for c in cmd) if isinstance(cmd, list) else cmd, flush=True)
    subprocess.run(cmd, check=True, **kw)


def sh(cmd):
    subprocess.run(cmd, shell=True, check=True)


def loudnorm(src, dst, i=-16.0):
    run(["ffmpeg", "-y", "-i", src, "-af",
         f"loudnorm=I={i}:TP=-1.5:LRA=11", "-ar", "48000", "-ac", "2", dst])


def write_srt(timeline, path):
    """One cue per sentence, timed proportionally inside each spoken line."""
    def fmt(t):
        h = int(t // 3600); m = int(t % 3600 // 60); s = int(t % 60)
        ms = int(round((t - int(t)) * 1000))
        return f"{h:02d}:{m:02d}:{s:02d},{ms:03d}"

    cues = []
    for seg in timeline["segments"]:
        parts = [p for p in re.split(r"(?<=[.?!]) +", seg["vo"].strip()) if p]
        dur = seg["end"] - seg["start"]
        total = sum(len(p) for p in parts) or 1
        tc = seg["start"]
        for p in parts:
            d = dur * len(p) / total
            cues.append((tc, tc + d, p))
            tc += d
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w") as f:
        for i, (a, b, text) in enumerate(cues, 1):
            f.write(f"{i}\n{fmt(a)} --> {fmt(b)}\n{text}\n\n")
    return len(cues)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--timeline", default=os.path.join(HERE, "timeline.json"))
    ap.add_argument("--output", default=os.path.join(HERE, "fsend-explainer.mp4"))
    ap.add_argument("--scale", type=int, default=2, help="capture supersample factor")
    ap.add_argument("--workers", type=int, default=min(6, max(1, (os.cpu_count() or 4) - 1)),
                    help="parallel capture processes")
    ap.add_argument("--crf", type=int, default=18)
    ap.add_argument("--grain", type=int, default=4, help="film grain strength (0 = off)")
    ap.add_argument("--skip-narration", action="store_true")
    ap.add_argument("--skip-capture", action="store_true")
    ap.add_argument("--no-music", action="store_true")
    ap.add_argument("--music-level", type=float, default=1.0)
    ap.add_argument("--preview", default=None,
                    help="comma-separated timestamps: write stills to preview/ and stop")
    ap.add_argument("--poster", default=None,
                    help="timestamp: rewrite the README thumbnail and stop")
    args = ap.parse_args()

    # 0) stills (visual iteration / README art — no audio, no encode) --------
    if args.preview is not None:
        run([PY, os.path.join(HERE, "capture.py"), "--preview", args.preview,
             "--scale", "1"])
        return
    if args.poster is not None:
        run([PY, os.path.join(HERE, "capture.py"), "--poster", args.poster,
             "--scale", str(args.scale)])
        return

    t0 = time.time()

    # 1) narration -> audio/*.wav + narration.wav + timeline.json -------------
    if not args.skip_narration:
        print("[1/6] narration")
        run([PY, os.path.join(HERE, "narration.py")])
    timeline = json.load(open(args.timeline))
    dur, fps = timeline["video_end"], timeline["fps"]
    total_frames = int(round(dur * fps))
    print(f"      {dur:.2f}s @ {fps}fps  = {total_frames} frames")

    # 2) frames --------------------------------------------------------------
    if not args.skip_capture:
        workers = max(1, args.workers)
        print(f"[2/6] capture frames  ({workers} worker{'s' if workers > 1 else ''})")
        if os.path.isdir(FRAMES):
            shutil.rmtree(FRAMES)
        os.makedirs(FRAMES, exist_ok=True)
        capture_py = os.path.join(HERE, "capture.py")
        if workers == 1:
            run([PY, capture_py, "--out", FRAMES, "--scale", str(args.scale)])
        else:
            # contiguous frame ranges -> one complete f%05d.png sequence
            bounds = [round(total_frames * i / workers) for i in range(workers + 1)]
            procs = [subprocess.Popen([PY, capture_py, "--out", FRAMES,
                                       "--scale", str(args.scale),
                                       "--frames", f"{bounds[i]}:{bounds[i + 1]}"])
                     for i in range(workers)]
            code = max(p.wait() for p in procs)
            if code != 0:
                sys.exit("capture failed")

    # 3) audio ---------------------------------------------------------------
    print("[3/6] audio")
    narration_norm = os.path.join(HERE, "narration_norm.wav")
    loudnorm(os.path.join(HERE, "narration.wav"), narration_norm)
    mix = narration_norm
    if not args.no_music:
        music = os.path.join(HERE, "music.wav")
        run([PY, os.path.join(HERE, "music.py"), "--duration", str(dur), "--out", music])
        mix = os.path.join(HERE, "mix.wav")
        run(["ffmpeg", "-y", "-i", narration_norm, "-i", music,
             "-filter_complex",
             f"[1:a]volume={args.music_level}[m];[0:a][m]amix=inputs=2:duration=first:normalize=0[a]",
             "-map", "[a]", "-ar", "48000", "-ac", "2", mix])

    # 4) video-only ----------------------------------------------------------
    print("[4/6] encode video")
    video_only = os.path.join(HERE, "video_only.mp4")
    # supersampled 2x frames -> 1080p (lanczos), then a whisper of film grain
    # (temporal, applied at final resolution — cheap, and better than a CSS grain
    # layer that the capture rasterizer would re-tile for every frame)
    vf = ("scale=1920:1080:flags=lanczos,"
          f"noise=alls={args.grain}:allf=t+u,"
          "format=yuv420p")
    run(["ffmpeg", "-y", "-framerate", str(fps), "-i", os.path.join(FRAMES, "f%05d.png"),
         "-vf", vf,
         "-c:v", "libx264", "-preset", "slow", "-crf", str(args.crf),
         "-profile:v", "high", "-level", "4.1", "-movflags", "+faststart", video_only])

    # 5) captions ------------------------------------------------------------
    print("[5/6] captions")
    n = write_srt(timeline, os.path.join(CAPTIONS, "fsend-explainer.srt"))
    print(f"      {n} cues -> captions/fsend-explainer.srt (sidecar, off by default)")

    # 6) mux -----------------------------------------------------------------
    print("[6/6] mux")
    os.makedirs(os.path.dirname(os.path.abspath(args.output)), exist_ok=True)
    run(["ffmpeg", "-y", "-i", video_only, "-i", mix,
         "-map", "0:v", "-map", "1:a", "-c:v", "copy", "-c:a", "aac", "-b:a", "192k",
         "-movflags", "+faststart", args.output])
    size = os.path.getsize(args.output) / 1e6
    print(f"\ndone in {time.time() - t0:.0f}s -> {args.output}  ({size:.1f} MB)")


if __name__ == "__main__":
    main()
