#!/usr/bin/env bash
#
# Regenerate the README demo (docs/assets/demo.svg).
#
# Records the scripted two-pane fsend transfer (demo.sh) as an asciinema cast,
# then renders it to an animated, razor-sharp SVG: Catppuccin-themed via
# svg-term, with JetBrains Mono embedded and a macOS + coral frame added in
# post. Vector means it stays crisp at any size and is decode-cheap on mobile.
#
# Requires: tmux, go, svg-term (npm i -g svg-term-cli), python3 + fonttools,
#           and the JetBrains Mono font (brew install --cask font-jetbrains-mono).
set -euo pipefail

SESSION=fsenddemo
OUT=docs/assets/demo.svg
HOLD_AFTER_SAVED=2   # let the final frame settle before killing the session

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

for tool in tmux go svg-term python3; do
  command -v "$tool" >/dev/null || { echo "missing required tool: $tool" >&2; exit 1; }
done
python3 -c "import fontTools" 2>/dev/null || { echo "missing python dep: fonttools (pip install fonttools brotli)" >&2; exit 1; }

CAST="$(mktemp -t fsend-demo-cast.XXXXXX)"
trap 'rm -f "$CAST"; tmux kill-session -t "$SESSION" 2>/dev/null || true' EXIT

# Build the binary, start the loopback server, lay out the tmux session.
scripts/demo/demo.sh setup

# Auto-stop: once the receiver shows "Saved", let it settle then kill the
# session so the recorder's `tmux attach` returns and recording ends. Hard cap
# at ~70s as a fallback.
(
  for _ in $(seq 1 70); do
    if tmux capture-pane -p -t "$SESSION.1" 2>/dev/null | grep -q "Saved"; then
      sleep "$HOLD_AFTER_SAVED"; break
    fi
    sleep 1
  done
  tmux kill-session -t "$SESSION" 2>/dev/null || true
) &

# Record the director-driven session, then render + frame the SVG.
python3 scripts/demo/ptyrec.py "$CAST"
python3 scripts/demo/svg-postprocess.py "$CAST" "$OUT"

echo "wrote $OUT ($(du -h "$OUT" | cut -f1))"
