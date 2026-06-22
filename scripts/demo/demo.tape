# VHS tape for the README demo.
#
#   regenerate with:  scripts/demo/record.sh
#
# This renders a 2x mp4 intermediate; record.sh rounds the outer corners and
# encodes the final animated WebP (docs/assets/demo.webp). Rendering at 2x and
# shipping WebP (full colour, smooth 8-bit alpha) keeps it crisp on retina and
# free of the banding/jaggies a 256-colour GIF would have.
#
# The actual choreography lives in demo.sh's "director"; this tape only sizes
# the terminal, kicks off the scripted session, and records it.
#
# Geometry is sized to ~2x the README's display width (820px) — just enough to
# stay crisp on retina without over-rendering. Animated-WebP playback is
# decode-bound (cost scales with pixels per frame, not file size), so the old
# ~2.9x render played slowly on weaker mobile CPUs; ~2x roughly halves the
# per-frame pixels while staying sharp at every display size.
#
# Apparent glyph size on the page is the display width (820) / columns, so it's
# set by the column count, NOT this resolution. Width hugs fsend's longest line
# — the receiver's one-line "Saved …" summary (97 cols, deterministic) — with
# ~3 cols of margin so it never wraps and the card carries no dead space. (cols
# = (Width-100)/15, rows = (Height-240)/30 for this font/padding/bar: ~100 x ~28.)

Output "/tmp/fsend-demo/demo.mp4"

Set FontFamily "JetBrains Mono"   # matches the fsend wordmark
Set FontSize 24
Set Width 1600
Set Height 1040
Set Padding 32
Set Margin 8
Set MarginFill "#E0805A"          # thin fsend-coral edge
Set BorderRadius 14
Set WindowBar Colorful
Set WindowBarSize 72              # bigger mac header / traffic-light dots
Set Theme "Catppuccin Mocha"
Set TypingSpeed 0ms

# Build the session and start the local server off-camera.
Hide
Type "scripts/demo/demo.sh setup" Enter
Sleep 3s
# Attach (still hidden); the director waits for this attach before typing.
Type "scripts/demo/demo.sh play" Enter
Sleep 800ms
Show

# The director now drives both panes: send, share code, accept, then the ~6s
# 500 MB transfer with its progress bar, then save. This window must outlast the
# whole sequence with margin — pairing/dial setup latency varies under render
# load, so a tight window can cut off mid-transfer. The director settles on the
# static "Saved" frame well before this elapses; record.sh then drops the
# duplicate tail and holds that final frame ~5s before the loop restarts.
Sleep 30s
