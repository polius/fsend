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
# Geometry below is 2x; keep it a clean doubling of the 1x design
# (FontSize 16, Width 1230, Height 780, Padding 24, Margin 6, Radius 10).
#
# Width fits the longest line fsend prints — the receiver's one-line "Saved …"
# summary (~104 cols) — with margin to spare, so it never wraps. (cols =
# (Width-160)/20, rows = (Height-320)/40 for this font/padding/margin:
# ~115 x ~31.)

Output "/tmp/fsend-demo/demo.mp4"

Set FontFamily "JetBrains Mono"   # matches the fsend wordmark
Set FontSize 32
Set Width 2460
Set Height 1560
Set Padding 48
Set Margin 12
Set MarginFill "#E0805A"          # thin fsend-coral edge
Set BorderRadius 20
Set WindowBar Colorful
Set WindowBarSize 80              # 2x default, so the traffic-light dots scale up
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
