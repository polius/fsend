#!/usr/bin/env bash
# Rebuild the fsend explainer end-to-end.
set -e
cd "$(dirname "$0")"

# narration — synthesize each line with the local Kokoro neural voice
.venv-tts/bin/python synth_kokoro.py   # audio/<id>.wav  (Kokoro, local)
REUSE_WAVS=1 python3 build_audio.py     # narration.wav + timeline.json
python3 render.py                      # frames/ (reads timeline.json)

# normalize voice to -16 LUFS, encode video-only stream
ffmpeg -y -i narration.wav -af "loudnorm=I=-16:TP=-1.5:LRA=11" -ar 48000 -ac 2 narration_norm.wav
ffmpeg -y -framerate 30 -i frames/f%05d.png -c:v libx264 -profile:v high \
  -pix_fmt yuv420p -crf 18 -movflags +faststart video_only.mp4

# subtitles: one sentence-cue per VO chunk, timed proportionally from timeline.json.
# Written into captions/ (NOT next to the mp4) so players don't auto-attach it —
# captions stay OFF by default, available to load or upload when wanted.
python3 - <<'PY'
import json, os, re
tl = json.load(open('timeline.json'))
def fmt(t):
    h=int(t//3600); m=int(t%3600//60); s=int(t%60); ms=int(round((t-int(t))*1000))
    return f"{h:02d}:{m:02d}:{s:02d},{ms:03d}"
cues=[]
for seg in tl['segments']:
    parts=[p for p in re.split(r'(?<=[.?!]) +', seg['vo'].strip()) if p]
    dur=seg['end']-seg['start']; tot=sum(len(p) for p in parts) or 1; tc=seg['start']
    for p in parts:
        d=dur*len(p)/tot; cues.append((tc, tc+d, p)); tc+=d
os.makedirs('captions', exist_ok=True)
with open('captions/fsend-explainer.srt','w') as f:
    for i,(a,b,t) in enumerate(cues,1):
        f.write(f"{i}\n{fmt(a)} --> {fmt(b)}\n{t}\n\n")
PY

# mux: video + voice only — no embedded subtitle track (ffmpeg's mp4 muxer always
# force-marks an embedded subtitle as default/on). Captions live in captions/.
ffmpeg -y -i video_only.mp4 -i narration_norm.wav \
  -map 0:v -map 1:a -c:v copy -c:a aac -b:a 192k \
  -movflags +faststart fsend-explainer.mp4

echo "done: fsend-explainer.mp4  (+ captions/fsend-explainer.srt — off by default)"
