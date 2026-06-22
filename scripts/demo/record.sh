#!/usr/bin/env bash
#
# Regenerate the README demo (docs/assets/demo.webp).
#
# Builds fsend, renders a 2x mp4 with VHS, rounds the outer corners with an
# anti-aliased alpha mask, and encodes an animated WebP. WebP (full colour,
# smooth 8-bit alpha) at 2x stays crisp on retina and avoids the banding and
# 1-bit-transparency jaggies a GIF would have.
#
# Requires: vhs, tmux, ffmpeg, img2webp (brew install vhs tmux ffmpeg webp).
set -euo pipefail

OUT=docs/assets/demo.webp
MP4=/tmp/fsend-demo/demo.mp4   # matches Output in demo.tape
FRAMES=/tmp/fsend-demo/frames
# Outer corner radius = tape Margin (8) + BorderRadius (14), so the coral
# frame's corners stay concentric with the inner window.
RADIUS=22
FRAME_MS=40      # 25 fps
END_HOLD_MS=5000 # the final frame is held this long before the loop restarts

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

for tool in vhs tmux go ffmpeg img2webp; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done

mkdir -p /tmp/fsend-demo
export FSEND_BIN=/tmp/fsend-demo/fsend
go build -o "$FSEND_BIN" ./cmd/fsend

# Pre-warm the 1 GiB transfer payload so the off-camera setup stays fast (it's
# cached, so this only pays the ~4 s generation cost on the first run).
scripts/demo/demo.sh payload

vhs scripts/demo/demo.tape

# Match the mask to the actual mp4 size (so tape geometry changes just work).
dims="$(ffprobe -v error -select_streams v:0 -show_entries stream=width,height -of csv=p=0:s=x "$MP4")"
W="${dims%x*}"; H="${dims#*x}"

# Anti-aliased rounded-rectangle alpha mask (gray = alpha), computed once. The
# +0.5px ramp at the radius boundary is what gives the corners smooth edges.
ffmpeg -y -f lavfi -i "color=c=black:s=${W}x${H}" -vf \
"geq=lum='clip((${RADIUS} - hypot(max(0\,${RADIUS}-min(X\,W-1-X))\,max(0\,${RADIUS}-min(Y\,H-1-Y))) + 0.5)*255\,0\,255)',format=gray" \
-frames:v 1 /tmp/fsend-demo/mask.png 2>/dev/null

# Punch the mask into every frame's alpha channel → RGBA PNGs.
rm -rf "$FRAMES"; mkdir -p "$FRAMES"
ffmpeg -y -i "$MP4" -i /tmp/fsend-demo/mask.png -filter_complex \
"[0:v]format=rgba[v];[v][1:v]alphamerge" -vsync 0 "$FRAMES/f%05d.png" 2>/dev/null

# The recording ends with a tail of identical "Saved" frames (the director
# holds the final state). Drop that tail and re-add the hold as a single
# long-delay frame, so the end-of-loop pause is exactly END_HOLD_MS — not
# "recorded tail + hold". Only trailing duplicates are dropped; the mid-demo
# reading pauses keep their frames. (Portable to bash 3.2 — no mapfile /
# negative indices.)
hash_of() { shasum "$1" | awk '{print $1}'; }
fl=( "$FRAMES"/f*.png )
n=${#fl[@]}
lasth="$(hash_of "${fl[$((n - 1))]}")"
start=$((n - 1))
i=$((n - 2))
while [ "$i" -ge 0 ] && [ "$(hash_of "${fl[$i]}")" = "$lasth" ]; do
  start=$i; i=$((i - 1))
done
head=( "${fl[@]:0:start}" )  # unique frames before the hold
hold="${fl[$start]}"         # one representative of the held final frame

# Assemble the animated WebP (infinite loop): every frame at FRAME_MS, then the
# held final frame at END_HOLD_MS.
img2webp -loop 0 -lossy -q 82 -m 6 \
  -d "$FRAME_MS" "${head[@]}" \
  -d "$END_HOLD_MS" "$hold" \
  -o "$OUT" >/dev/null 2>&1
rm -rf "$FRAMES"

echo "wrote $OUT ($(du -h "$OUT" | cut -f1))"
