#!/usr/bin/env python3
# Turn the recorded cast into the final README demo SVG.
#
# svg-term gives us a razor-sharp, Catppuccin-themed animation but five things
# it can't do itself, applied here in order:
#   1. End-of-loop hold   — svg-term has no "pause on the last frame" option, so
#                           we rescale its @keyframes timeline to hold the final
#                           "Saved" frame HOLD_S seconds before looping.
#   2. Column grid        — svg-term lays text out on a 1.002-unit column pitch
#                           but spaces its filmstrip frames a clean 100 units
#                           apart, so a full-width line spills into the frame
#                           behind it. See fit_grid() and clip_frames().
#   3. Title bar          — a macOS title bar (traffic lights + the "> fsend_"
#                           wordmark), drawn as static vector shapes in
#                           svg-term's spare top strip.
#   4. Embedded font      — JetBrains Mono (the fsend wordmark font), subset to
#                           the glyphs used and inlined as woff2 data-URIs, so it
#                           renders even where the font isn't installed (GitHub).
#   5. Coral window frame — a coral rounded outer frame around the window.
#
# Every assumption about svg-term's output is asserted, so a future svg-term
# version that changes its format fails loudly here instead of silently shipping
# a broken demo. Tuning knobs are the constants below.
#
#   python3 scripts/preview/svg-postprocess.py <in.cast> <out.svg>
import os, re, io, sys, html, base64, glob, subprocess
from fontTools import subset

# --- tuning knobs -----------------------------------------------------------
HOLD_S   = 6.0          # seconds to hold the final frame before the loop restarts
CORAL    = "#e0805a"    # fsend coral edge (matches the wordmark)
MARGIN   = 5.5          # coral border thickness (px, in window space)
WIN_R    = 12           # window corner radius
DOT_R    = 7.5          # traffic-light radius
DOTS     = [(23, "#ff5f58"), (47, "#ffbd2e"), (71, "#28c840")]  # cx, colour
DOT_CX0, DOT_GAP = 23, 24   # first traffic-light centre + spacing
# Title bar: fill + typography (Catppuccin Mocha tones so it stays in-family).
TITLE_BG = "#181825"    # mantle — a hair darker than the terminal base
HAIRLINE = "#313244"    # surface0 — a 1px rule under the bar
WORD_FG  = "#faf7f5"    # off-white, matched to the icon wordmark
COLS, ROWS, PADDING = 100, 34, 14
# svg-term's column pitch in terminal units: its font metric (0.6em at the size
# it picks) rounded to three decimals. Frames are spaced a clean 100, so the
# 0.002 overshoot is what fit_grid() takes back out.
CELL = 1.002
# ---------------------------------------------------------------------------

HERE = os.path.dirname(os.path.abspath(__file__))
PROFILE = os.path.join(HERE, "catppuccin-mocha.xresources")
SVG_TERM_FONT = "Monaco,Consolas,Menlo,'Bitstream Vera Sans Mono','Powerline Symbols',monospace"
FAMILY = "'JetBrains Mono',monospace"


def trim(v):
    """Format an SVG coordinate the way svg-term does: 3 decimals, no padding."""
    return f"{v:.3f}".rstrip("0").rstrip(".") or "0"


def find_font(style):
    """Locate JetBrainsMono-<style>.ttf across the usual install locations."""
    roots = [os.path.expanduser("~/Library/Fonts"), "/Library/Fonts",
             "/opt/homebrew/Caskroom/font-jetbrains-mono", "/usr/share/fonts",
             os.path.expanduser("~/.local/share/fonts")]
    for root in roots:
        hits = glob.glob(os.path.join(root, f"**/JetBrainsMono-{style}.ttf"), recursive=True)
        if hits:
            return hits[0]
    sys.exit(f"error: JetBrainsMono-{style}.ttf not found "
             f"(brew install --cask font-jetbrains-mono)")


def run_svg_term(cast, out):
    subprocess.run(["svg-term", "--in", cast, "--out", out, "--window",
                    "--width", str(COLS), "--height", str(ROWS),
                    "--padding", str(PADDING),
                    "--no-cursor",   # a stray blinking block adds noise; the
                                     # motion (typing + progress) carries the life
                    "--term", "xresources", "--profile", PROFILE], check=True)


def anim_name(s):
    """The @keyframes/animation-name svg-term chose (its minifier renames it
    per-run, so we must read it rather than assume a fixed letter)."""
    m = re.search(r"animation-name:([A-Za-z_][\w-]*)", s)
    assert m, "svg-term output changed: no animation-name property"
    return m.group(1)


def title_h(s):
    """Height of the spare strip svg-term leaves above the terminal (the
    traffic-light bar) — read from the terminal <svg y=...><style>."""
    m = re.search(r'<svg[^>]*\by="([\d.]+)"><style>', s)
    assert m, "svg-term output changed: could not locate the terminal offset"
    return float(m.group(1))


def extend_hold(s):
    """Rescale svg-term's @keyframes so the final frame holds HOLD_S seconds."""
    name = anim_name(s)
    dur = float(re.search(r"animation-duration:([\d.]+)s", s).group(1))
    block = re.search(r"@keyframes " + re.escape(name) + r"\{.*?\}\s*\}", s, re.S)
    assert block, f"svg-term output changed: no @keyframes {name} block"
    block = block.group(0)
    pairs = [(float(p), int(v)) for p, v in
             re.findall(r"([\d.]+)%\{transform:translateX\((-?\d+)px\)\}", block)]
    assert pairs, "svg-term output changed: no translateX keyframes"
    minval = min(v for _, v in pairs)                 # most-scrolled = final frame
    p_start = min(p for p, v in pairs if v == minval)  # when content stops moving
    last_t = p_start / 100 * dur
    new_dur = last_t + HOLD_S
    factor = dur / new_dur
    new_block = re.sub(r"([\d.]+)%\{", lambda m: f"{float(m.group(1)) * factor:.4g}%{{", block)
    s = s.replace(block, new_block, 1)
    s = s.replace(f"animation-duration:{dur:g}s", f"animation-duration:{new_dur:.4g}s", 1)
    print(f"  hold: {dur:.2f}s total -> {new_dur:.2f}s (final frame holds {HOLD_S:g}s)", file=sys.stderr)
    return s


def fit_grid(s):
    """Rescale every text run's x so the column grid spans the frame exactly.

    svg-term places a column of text every 1.002 units — its font metric
    (0.6em at the size it picks, 1.0014 units) rounded to three decimals — but
    spaces its filmstrip frames exactly 100 units apart. Over 100 columns that
    leaves the last column ~0.2 units (two pixels) past the end of its own
    frame, and since the previous frame sits exactly 100 units to the left, its
    tail lands on the left edge of the current frame: a stray glyph fragment
    that reads as a ":" floating in front of the progress bar, on exactly the
    rows whose line fills the terminal (the mpb bar always does).

    Snapping column c back to x == c makes the grid end flush with the frame,
    so nothing crosses the boundary. Glyphs come out 0.2% narrower, which is
    invisible.
    """
    xs = [float(x) for x in re.findall(r'<text x="([\d.]+)"', s)]
    assert xs, "svg-term output changed: no positioned text runs"
    err = max(abs(x / CELL - round(x / CELL)) for x in xs)
    assert err < 0.005, f"svg-term output changed: runs are off the {CELL} column pitch"
    s = re.sub(r'<text x="([\d.]+)"',
               lambda m: '<text x="%s"' % trim(float(m.group(1)) / CELL), s)
    print(f"  grid: snapped {len(xs)} text runs from a {CELL} to a 1.0 column pitch",
          file=sys.stderr)
    return s


def clip_frames(s):
    """Pin each filmstrip frame to its own viewport, so none can bleed across.

    A belt to fit_grid()'s braces: after the grid snap no run reaches the frame
    edge, but the font metric is the renderer's call, and a renderer that draws
    a hair wider would resurrect the ghost. An explicit viewport clips any such
    overhang at the last column, where a real terminal clips it, rather than
    letting it land on the neighbouring frame.
    """
    m = re.search(r'<svg width="(\d+)">(<svg>)', s)
    assert m, "svg-term output changed: filmstrip frames not found"
    s = s[:m.start(2)] + f'<svg width="{COLS}">' + s[m.end(2):]
    xs = [int(x) for x in re.findall(r'<svg x="(\d+)">', s)]
    assert xs and all(x % COLS == 0 for x in xs), \
        "svg-term output changed: unexpected frame offsets"
    s = re.sub(r'<svg x="(\d+)">', rf'<svg width="{COLS}" x="\1">', s)
    return s


def _title_bar(w, h):
    """Traffic lights + '> fsend_' wordmark, in the top strip.

    Drawn as flat vector so it stays crisp and needs no font metrics beyond the
    known monospace advance (0.6em). Coordinates are in the window's own space.
    """
    f = 26.0                       # wordmark size
    adv = f * 0.6                  # JetBrains Mono advance width
    cy = h / 2.0                   # vertical centre of the strip
    base = cy + 0.365 * f          # baseline that centres caps optically
    chev_x = 104.0
    fs_x = chev_x + adv + f * 0.20
    cur_x = fs_x + 5 * adv + f * 0.12
    parts = [
        # bar fill — rounded top corners only (square off the bottom ones)
        f'<rect x="0" y="0" width="{w:g}" height="{h:g}" rx="{WIN_R}" ry="{WIN_R}" fill="{TITLE_BG}"/>',
        f'<rect x="0" y="{h - WIN_R:g}" width="{w:g}" height="{WIN_R:g}" fill="{TITLE_BG}"/>',
        f'<rect x="0" y="{h - 1:g}" width="{w:g}" height="1" fill="{HAIRLINE}"/>',
        f'<text x="{chev_x:g}" y="{base:.2f}" font-family="{FAMILY}" font-size="{f:g}"'
        f' font-weight="700" fill="{CORAL}">&gt;</text>',
        f'<text x="{fs_x:.2f}" y="{base:.2f}" font-family="{FAMILY}" font-size="{f:g}"'
        f' font-weight="500" fill="{WORD_FG}">fsend</text>',
        f'<rect x="{cur_x:.2f}" y="{base - 0.10 * f:.2f}" width="{0.46 * f:g}" height="{0.10 * f:g}"'
        f' rx="{0.035 * f:g}" fill="{CORAL}"/>',
    ]
    return "".join(parts)


def add_chrome(s):
    """Title bar, larger traffic lights, and a coral rounded outer frame."""
    h = title_h(s)
    W, _ = (float(x) for x in re.search(
        r'<svg[^>]*\bwidth="([\d.]+)"[^>]*\bheight="([\d.]+)"', s).groups())
    assert 'rx="5" ry="5" class="a"' in s, "svg-term output changed: window bg rect not found"
    s = s.replace('rx="5" ry="5" class="a"', f'rx="{WIN_R}" ry="{WIN_R}" class="a"', 1)
    # title bar sits directly under the window bg, so the dots draw on top of it
    s = re.sub(r'(<rect[^>]*\bclass="a"/>)(<svg y="0%")',
               lambda m: m.group(1) + _title_bar(W, h) + m.group(2), s, count=1)
    # replace svg-term's small dots (cx 20/40/60, cy 20, r 6) with larger ones,
    # vertically centred in the title bar
    dot_cy = h / 2.0
    for i, (old_cx, _) in enumerate([(20, 0), (40, 0), (60, 0)]):
        old = re.search(rf'<circle cx="{old_cx}" cy="20" r="6" fill="#[0-9a-f]+"/>', s)
        assert old, f"svg-term output changed: traffic light at cx={old_cx} not found"
        cx, colour = DOT_CX0 + i * DOT_GAP, DOTS[i][1]
        s = s.replace(old.group(0),
                      f'<circle cx="{cx}" cy="{dot_cy:g}" r="{DOT_R}" fill="{colour}"/>', 1)
    # nest the (now framed) window inside a coral outer canvas
    _, H = (float(x) for x in re.search(
        r'<svg[^>]*\bwidth="([\d.]+)"[^>]*\bheight="([\d.]+)"', s).groups())
    OW, OH, R = W + 2 * MARGIN, H + 2 * MARGIN, WIN_R + MARGIN
    s = re.sub(r"(<svg\b)", rf'\1 x="{MARGIN:g}" y="{MARGIN:g}"', s, count=1)
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{OW:g}" height="{OH:g}" '
            f'viewBox="0 0 {OW:g} {OH:g}"><rect width="{OW:g}" height="{OH:g}" '
            f'rx="{R:g}" ry="{R:g}" fill="{CORAL}"/>' + s + "</svg>")


def embed_fonts(s):
    """Subset JetBrains Mono to the glyphs used and inline as woff2 data-URIs."""
    chars = set(" ")
    for m in re.findall(r"<text[^>]*>([^<]*)</text>", s):
        chars |= set(html.unescape(m))
    codepoints = sorted(ord(c) for c in chars if ord(c) >= 0x20 or c == "\t")

    def woff2(path):
        opts = subset.Options(flavor="woff2", desubroutinize=True,
                              layout_features=["*"], notdef_outline=True)
        font = subset.load_font(path, opts)
        ss = subset.Subsetter(options=opts)
        ss.populate(unicodes=codepoints)
        ss.subset(font)
        buf = io.BytesIO()
        subset.save_font(font, buf, opts)
        return base64.b64encode(buf.getvalue()).decode("ascii")

    faces = "".join(
        f"@font-face{{font-family:'JetBrains Mono';font-style:normal;font-weight:{w};"
        f"src:url(data:font/woff2;base64,{woff2(find_font(style))}) format('woff2')}}"
        for w, style in ((400, "Regular"), (500, "Medium"), (700, "Bold")))
    assert SVG_TERM_FONT in s, "svg-term output changed: font-family stack not found"
    s = re.sub(r"(<style[^>]*>)", r"\1" + faces.replace("\\", "\\\\"), s, count=1)
    s = s.replace(SVG_TERM_FONT, FAMILY)
    print(f"  font: embedded Regular+Medium+Bold subset ({len(codepoints)} glyphs)", file=sys.stderr)
    return s


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: svg-postprocess.py <in.cast> <out.svg>")
    cast, out = sys.argv[1], sys.argv[2]
    raw = out + ".raw"
    run_svg_term(cast, raw)
    s = open(raw, encoding="utf-8").read()
    os.remove(raw)
    s = extend_hold(s)
    s = clip_frames(s)
    s = fit_grid(s)
    s = add_chrome(s)      # before embed_fonts, so the subset covers the chrome text
    s = embed_fonts(s)
    open(out, "w", encoding="utf-8").write(s)
    print(f"wrote {out} ({len(s.encode()) // 1024} KiB)", file=sys.stderr)


if __name__ == "__main__":
    main()
