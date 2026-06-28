#!/usr/bin/env python3
"""Lay out the narration timeline from per-segment audio and assemble narration.wav.

Audio-first: each VO line is measured and the visual timeline is laid out to match,
so the animation tracks the voice. With REUSE_WAVS=1 it measures the wavs already
synthesized by synth_kokoro.py (the neural voice); otherwise it falls back to macOS
`say` (Samantha) for quick offline tests only.
Outputs: narration.wav (48k stereo) + timeline.json
"""
import json
import os
import subprocess

HERE = os.path.dirname(os.path.abspath(__file__))
AUD = os.path.join(HERE, "audio")
os.makedirs(AUD, exist_ok=True)

VOICE = "Samantha"
RATE = 170                  # words per minute — calm, promo pace
FPS = 30

LEAD_IN = 0.5              # silence before first line
GAP_SAME = 0.34           # pause between lines in the same scene
GAP_SCENE = 0.85          # pause when the scene changes (breathing room)
TAIL = 4.0                # hold after the last line (longer outro)

# extra hold before a given line (on top of the normal gap) — give the viewer
# time to read the two-terminal demo before it moves on.
GAP_EXTRA = {
    "cmd2":   0.8,        # let the Sender block be read before the Receiver appears
    "modes1": 1.0,        # hold the finished "That's all" demo before the overview
}

# id, scene, beat, voiceover (jargon-light). Each scene draws its own on-screen text.
SEGMENTS = [
    ("brand1", "brand", 0,
     "Presenting fsend — a command-line tool that sends files and folders straight from one computer to another."),
    ("cmd1", "cmd", 0,
     "You point fsend at a file, and it hands you a short code."),
    ("cmd2", "cmd", 1,
     "Your friend runs that code, and the file lands on their machine."),
    ("modes1", "modes", 0,
     "Behind the scenes, fsend finds the best path on its own. First it tries your local network. If that's not possible, it connects directly across the internet. And only if a strict network blocks that, it falls back to an encrypted relay."),
    ("local1", "local", 0,
     "Here's how each one works. On the same local network, fsend finds the other device and connects directly — no server involved."),
    ("local2", "local", 1,
     "The server is never even contacted. It can be completely offline."),
    ("direct1", "direct", 0,
     "If the two devices aren't on the same local network, fsend connects them directly, across the internet. The server only helps them find each other."),
    ("direct2", "direct", 1,
     "Then it steps aside. Your bytes flow straight, peer to peer, across the internet."),
    ("relay1", "relay", 0,
     "And if a direct connection isn't possible either, fsend falls back to a relay. The server forwards the data between them."),
    ("relay2", "relay", 1,
     "But only as sealed, encrypted packets."),
    ("cta1", "cta", 0,
     "The free public server needs zero setup."),
    ("cta2", "cta", 1,
     "But if you want full control, run your own with a single command. Or just pull the Docker image."),
    ("outro1", "outro", 0,
     "fsend."),
    ("outro2", "outro", 1,
     "Simple, private and free."),
]


def run(cmd):
    subprocess.run(cmd, check=True, stdout=subprocess.DEVNULL,
                   stderr=subprocess.DEVNULL)


def duration(path):
    out = subprocess.check_output([
        "ffprobe", "-v", "error", "-show_entries", "format=duration",
        "-of", "default=noprint_wrappers=1:nokey=1", path])
    return float(out.strip())


def synth():
    # REUSE_WAVS=1 -> audio/<id>.wav already produced (e.g. by synth_kokoro.py);
    # just measure them. Otherwise fall back to macOS `say`.
    reuse = os.environ.get("REUSE_WAVS") == "1"
    durs = []
    for (sid, scene, beat, vo) in SEGMENTS:
        wav = os.path.join(AUD, f"{sid}.wav")
        if not reuse:
            aiff = os.path.join(AUD, f"{sid}.aiff")
            run(["say", "-v", VOICE, "-r", str(RATE), "-o", aiff, vo])
            run(["ffmpeg", "-y", "-i", aiff, "-ar", "48000", "-ac", "2", wav])
        durs.append(duration(wav))
    return durs


def make_silence(sec):
    sec = round(sec, 3)
    path = os.path.join(AUD, f"sil_{int(sec*1000)}.wav")
    if not os.path.exists(path):
        run(["ffmpeg", "-y", "-f", "lavfi", "-t", f"{sec}",
             "-i", "anullsrc=r=48000:cl=stereo", path])
    return path


def main():
    durs = synth()

    # ---- lay out the timeline from real audio durations ----
    segs = []
    concat_parts = [make_silence(LEAD_IN)]
    t = LEAD_IN
    for i, (sid, scene, beat, vo) in enumerate(SEGMENTS):
        if i > 0:
            prev_scene = SEGMENTS[i - 1][1]
            gap = GAP_SCENE if scene != prev_scene else GAP_SAME
            gap += GAP_EXTRA.get(sid, 0.0)
            concat_parts.append(make_silence(gap))
            t += gap
        start = t
        dur = durs[i]
        concat_parts.append(os.path.join(AUD, f"{sid}.wav"))
        t += dur
        segs.append({"id": sid, "scene": scene, "beat": beat,
                     "start": round(start, 3), "dur": round(dur, 3),
                     "end": round(start + dur, 3), "vo": vo})
    concat_parts.append(make_silence(TAIL))
    video_end = round(t + TAIL, 3)

    # ---- assemble narration.wav ----
    listfile = os.path.join(AUD, "concat.txt")
    with open(listfile, "w") as f:
        for p in concat_parts:
            f.write(f"file '{p}'\n")
    narration = os.path.join(HERE, "narration.wav")
    run(["ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", listfile,
         "-ar", "48000", "-ac", "2", narration])

    # ---- scene windows (boundaries centered in scene-change gaps) ----
    scenes = []
    order = []
    for s in segs:
        if not order or order[-1]["name"] != s["scene"]:
            order.append({"name": s["scene"], "first": s["start"],
                          "last_end": s["end"]})
        else:
            order[-1]["last_end"] = s["end"]
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

    print(f"narration: {timeline['narration_dur']:.2f}s   video_end: {video_end:.2f}s")
    for sc in scenes:
        print(f"  {sc['name']:7s} {sc['start']:6.2f} -> {sc['end']:6.2f}")


if __name__ == "__main__":
    main()
