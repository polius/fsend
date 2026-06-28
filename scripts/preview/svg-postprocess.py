#!/usr/bin/env python3
# Turn the recorded cast into the final README demo SVG.
#
# svg-term gives us a razor-sharp, Catppuccin-themed animation but three things
# it can't do itself, applied here in order:
#   1. End-of-loop hold   — svg-term has no "pause on the last frame" option, so
#                           we rescale its @keyframes timeline to hold the final
#                           "Saved" frame HOLD_S seconds before looping.
#   2. Embedded font      — JetBrains Mono (the fsend wordmark font), subset to
#                           the glyphs used and inlined as woff2 data-URIs, so it
#                           renders even where the font isn't installed (GitHub).
#   3. macOS + coral chrome — a coral rounded outer frame and larger traffic
#                           lights, drawn as static vector shapes around the
#                           untouched animation.
#
# Every assumption about svg-term's output is asserted, so a future svg-term
# version that changes its format fails loudly here instead of silently shipping
# a broken demo. Tuning knobs are the constants below.
#
#   python3 scripts/preview/svg-postprocess.py <in.cast> <out.svg>
import os, re, io, sys, html, base64, glob, subprocess
from fontTools import subset

# --- tuning knobs -----------------------------------------------------------
HOLD_S   = 8.0          # seconds to hold the final frame before the loop restarts
CORAL    = "#e0805a"    # fsend coral edge (matches the wordmark)
MARGIN   = 5.5          # coral border thickness (px, in window space)
WIN_R    = 12           # window corner radius
DOT_R    = 7.5          # traffic-light radius
DOTS     = [(23, "#ff5f58"), (47, "#ffbd2e"), (71, "#28c840")]  # cx, colour
DOT_CY   = 23
COLS, ROWS, PADDING = 100, 28, 14
# ---------------------------------------------------------------------------

HERE = os.path.dirname(os.path.abspath(__file__))
PROFILE = os.path.join(HERE, "catppuccin-mocha.xresources")
SVG_TERM_FONT = "Monaco,Consolas,Menlo,'Bitstream Vera Sans Mono','Powerline Symbols',monospace"


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
                    "--term", "xresources", "--profile", PROFILE], check=True)


def extend_hold(s):
    """Rescale svg-term's @keyframes so the final frame holds HOLD_S seconds."""
    dur = float(re.search(r"animation-duration:([\d.]+)s", s).group(1))
    block = re.search(r"@keyframes p\{.*?\}\s*\}", s, re.S)
    assert block, "svg-term output changed: no @keyframes p block"
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
        for w, style in ((400, "Regular"), (700, "Bold")))
    assert SVG_TERM_FONT in s, "svg-term output changed: font-family stack not found"
    s = re.sub(r"(<style[^>]*>)", r"\1" + faces.replace("\\", "\\\\"), s, count=1)
    s = s.replace(SVG_TERM_FONT, "'JetBrains Mono',monospace")
    print(f"  font: embedded Regular+Bold subset ({len(codepoints)} glyphs)", file=sys.stderr)
    return s


def add_chrome(s):
    """Coral rounded frame + larger macOS traffic lights around the animation."""
    assert 'rx="5" ry="5" class="a"' in s, "svg-term output changed: window bg rect not found"
    s = s.replace('rx="5" ry="5" class="a"', f'rx="{WIN_R}" ry="{WIN_R}" class="a"', 1)
    # replace svg-term's small dots (cx 20/40/60, r 6) with larger, accurate ones
    for (old_cx, _), (cx, colour) in zip([(20, 0), (40, 0), (60, 0)], DOTS):
        old = re.search(rf'<circle cx="{old_cx}" cy="20" r="6" fill="#[0-9a-f]+"/>', s)
        assert old, f"svg-term output changed: traffic light at cx={old_cx} not found"
        s = s.replace(old.group(0), f'<circle cx="{cx}" cy="{DOT_CY}" r="{DOT_R}" fill="{colour}"/>', 1)
    # nest the (now framed) window inside a coral outer canvas
    mw = re.search(r'<svg[^>]*\bwidth="([\d.]+)"[^>]*\bheight="([\d.]+)"', s)
    W, H = float(mw.group(1)), float(mw.group(2))
    OW, OH, R = W + 2 * MARGIN, H + 2 * MARGIN, WIN_R + MARGIN
    s = re.sub(r"(<svg\b)", rf'\1 x="{MARGIN:g}" y="{MARGIN:g}"', s, count=1)
    return (f'<svg xmlns="http://www.w3.org/2000/svg" width="{OW:g}" height="{OH:g}" '
            f'viewBox="0 0 {OW:g} {OH:g}"><rect width="{OW:g}" height="{OH:g}" '
            f'rx="{R:g}" ry="{R:g}" fill="{CORAL}"/>' + s + "</svg>")


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: svg-postprocess.py <in.cast> <out.svg>")
    cast, out = sys.argv[1], sys.argv[2]
    raw = out + ".raw"
    run_svg_term(cast, raw)
    s = open(raw, encoding="utf-8").read()
    os.remove(raw)
    s = extend_hold(s)
    s = embed_fonts(s)
    s = add_chrome(s)
    open(out, "w", encoding="utf-8").write(s)
    print(f"wrote {out} ({len(s.encode()) // 1024} KiB)", file=sys.stderr)


if __name__ == "__main__":
    main()
