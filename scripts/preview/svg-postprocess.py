#!/usr/bin/env python3
# Turn the recorded cast into the final README demo SVG, in a dark or a light
# theme.
#
# The cast is recorded with fsend's dark palette in truecolour. For the light
# SVG, every colour is first mapped to its light-column sibling (the same 1:1
# table internal/uxlog/glyphs.go defines), so both variants come from one
# recording. svg-term then renders a razor-sharp animation, and we add what it
# can't do itself, in order:
#   1. Column grid        — svg-term lays text out on a 1.002-unit column pitch
#                           but spaces its filmstrip frames a whole number of
#                           columns apart, so long runs drift off the grid (a
#                           box's right edge misses its corners) and full-width
#                           lines spill into the frame behind. See fit_grid(),
#                           exact_pitch() and clip_frames().
#   2. Cursor             — a cell-sized block in the theme's foreground, shown
#                           only where someone is typing.
#   3. End-of-loop hold   — svg-term has no "pause on the last frame" option, so
#                           we rescale its @keyframes timeline to hold the final
#                           "Saved" frame HOLD_S seconds before looping.
#   4. Motion             — the loop fades out and back in instead of cutting,
#                           and prefers-reduced-motion shows the final frame.
#   5. Title bar          — a macOS title bar (traffic lights + the "> fsend_"
#                           wordmark), drawn as static vector shapes in
#                           svg-term's spare top strip.
#   6. Embedded font      — JetBrains Mono (the fsend wordmark font), subset to
#                           the glyphs used and inlined as woff2 data-URIs, so it
#                           renders even where the font isn't installed (GitHub).
#   7. Coral window frame — a coral rounded outer frame around the window.
#
# Every assumption about svg-term's output is asserted, so a future svg-term
# version that changes its format fails loudly here instead of silently shipping
# a broken demo. Tuning knobs are the constants below.
#
#   python3 scripts/preview/svg-postprocess.py [--theme dark|light] <in.cast> <out.svg>
import os, re, io, sys, json, html, base64, glob, tempfile, subprocess
from fontTools import subset

# --- tuning knobs -----------------------------------------------------------
HOLD_S   = 6.0          # seconds to hold the final frame before the loop restarts
FADE_IN_S, FADE_OUT_S = 0.4, 0.8   # the loop's soft restart (out ends the hold)
CORAL    = "#e0805a"    # fsend coral edge (matches the wordmark)
MARGIN   = 5.5          # coral border thickness (px, in window space)
WIN_R    = 12           # window corner radius
DOT_R    = 7.5          # traffic-light radius
DOTS     = [(23, "#ff5f58"), (47, "#ffbd2e"), (71, "#28c840")]  # cx, colour
DOT_CX0, DOT_GAP = 23, 24   # first traffic-light centre + spacing
# Must match demo.sh / ptyrec.py, which read the same variables.
COLS = int(os.environ.get("FSEND_DEMO_COLS", 88))
ROWS = int(os.environ.get("FSEND_DEMO_ROWS", 33))
PADDING = 14
# svg-term's column pitch in terminal units: its font metric (0.6em at the size
# it picks) rounded to three decimals. Frames are spaced a whole number of
# columns apart, so the 0.002 overshoot is what fit_grid() takes back out.
CELL = 1.002

# The terminal base is Tokyo Night (dark) / Tokyo Night Day (light): the family
# fsend's six colour roles come from. TOKENS maps each dark-column hex in the
# cast to its light sibling; the first six rows are glyphs.go's roles verbatim.
TOKENS = {
    "#ff9e64": "#b15c00",   # accent
    "#bb9af7": "#7847bd",   # primary
    "#9ece6a": "#587539",   # success
    "#e0af68": "#8c6c3e",   # warning
    "#f7768e": "#b3261e",   # error
    "#7dcfff": "#007197",   # info
    "#7aa2f7": "#3760bf",   # shell prompt (demo.sh PROMPT_RGB)
    "#9aa5ce": "#6172b0",   # card host text (demo.sh HOST_RGB)
    "#e0805a": "#e0805a",   # brand/logo-orange: fixed in both themes
}
THEMES = {
    "dark": dict(bg="#1a1b26", fg="#c0caf5", title_bg="#16161e", hairline="#292e42",
                 word_fg="#faf7f5",   # icon-dark.svg wordmark
                 ansi=["#15161e", "#f7768e", "#9ece6a", "#e0af68", "#7aa2f7", "#bb9af7",
                       "#7dcfff", "#a9b1d6", "#414868", "#f7768e", "#9ece6a", "#e0af68",
                       "#7aa2f7", "#bb9af7", "#7dcfff", "#c0caf5"]),
    "light": dict(bg="#ffffff", fg="#343b58", title_bg="#f2f3f7", hairline="#dfe1e8",
                  word_fg="#1a1410",  # icon-light.svg wordmark
                  ansi=["#e9e9ed", "#b3261e", "#587539", "#8c6c3e", "#3760bf", "#7847bd",
                        "#007197", "#6172b0", "#a1a6c5", "#b3261e", "#587539", "#8c6c3e",
                        "#3760bf", "#7847bd", "#007197", "#343b58"]),
}
# ---------------------------------------------------------------------------

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


def recolor_cast(cast, theme):
    """Copy the cast with every truecolour SGR mapped to the theme's column.

    Unknown colours fail loudly: a new colour in fsend's output must get a
    light sibling in TOKENS rather than ship unmapped in the light SVG.
    """
    def swap(m):
        hexc = "#%02x%02x%02x" % tuple(int(v) for v in m.group(2, 3, 4))
        assert hexc in TOKENS, f"cast uses {hexc}, which has no entry in TOKENS"
        out = TOKENS[hexc] if theme == "light" else hexc
        return m.group(1) + ";".join(str(int(out[i:i + 2], 16)) for i in (1, 3, 5))
    sgr = re.compile(r"(38;2;)(\d+);(\d+);(\d+)")
    lines = open(cast, encoding="utf-8").read().splitlines()
    events = [json.loads(l) for l in lines[1:]]
    for e in events:
        e[2] = sgr.sub(swap, e[2])
    fd, path = tempfile.mkstemp(suffix=".cast")
    with os.fdopen(fd, "w", encoding="utf-8") as f:
        f.write(lines[0] + "\n" + "".join(json.dumps(e) + "\n" for e in events))
    return path


def run_svg_term(cast, out, theme):
    t = THEMES[theme]
    fd, profile = tempfile.mkstemp(suffix=".xresources")
    with os.fdopen(fd, "w") as f:
        f.write(f"*.foreground: {t['fg']}\n*.background: {t['bg']}\n"
                f"*.cursorColor: {t['fg']}\n*.colorBD: {t['fg']}\n")
        f.writelines(f"*.color{i}: {c}\n" for i, c in enumerate(t["ansi"]))
    try:
        subprocess.run(["svg-term", "--in", cast, "--out", out, "--window",
                        "--width", str(COLS), "--height", str(ROWS),
                        "--padding", str(PADDING),
                        "--term", "xresources", "--profile", profile], check=True)
    finally:
        os.remove(profile)


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


def filmstrip(s):
    """(@keyframes block, duration, final translateX) of svg-term's animation."""
    name = anim_name(s)
    dur = float(re.search(r"animation-duration:([\d.]+)s", s).group(1))
    block = re.search(r"@keyframes " + re.escape(name) + r"\{.*?\}\s*\}", s, re.S)
    assert block, f"svg-term output changed: no @keyframes {name} block"
    # Percentage stops only: svg-term may close with a `to{}` stop, which
    # steps(1,end) never shows.
    xs = [int(v) for v in re.findall(r"%\{transform:translateX\((-?\d+)px\)", block.group(0))]
    assert xs, "svg-term output changed: no translateX keyframes"
    return block.group(0), dur, min(xs)


def extend_hold(s):
    """Rescale svg-term's @keyframes so the final frame holds HOLD_S seconds."""
    block, dur, minval = filmstrip(s)   # most-scrolled = final frame
    pairs = [(float(p), int(v)) for p, v in
             re.findall(r"([\d.]+)%\{transform:translateX\((-?\d+)px\)\}", block)]
    # The final frame starts at the first stop that already looks like it:
    # the recording runs on past "Saved", and those tail frames differ from
    # it at most invisibly (a redraw, a pruned cursor).
    body = {int(x or 0): b for x, b in
            re.findall(rf'<svg width="{COLS}"(?: x="(\d+)")?>(.*?)</svg>', s, re.S)}
    assert -minval in body, "svg-term output changed: final frame not found"
    p_start = min(p for p, v in pairs if body.get(-v) == body[-minval])
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

    Snapping column c back to x == c puts every run's start on the grid;
    exact_pitch() then makes the glyphs inside a run advance on it too.
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


def exact_pitch(s):
    """Size the font so a glyph advances exactly one column.

    svg-term rounds its font size to 1.67, so each JetBrains Mono glyph
    (0.6em) advances 1.002 units: a 38-glyph box border ends 0.07 units past
    the column where the row below draws its right-hand edge, and the corner
    visibly misses the line. At 5/3 the advance is exactly 1.0, so box
    corners meet their edges and long rules end flush with the frame.
    """
    assert 'font-size="1.67"' in s, "svg-term output changed: font size is not 1.67"
    return s.replace('font-size="1.67"', 'font-size="1.66667"', 1)


def style_cursor(s, theme):
    """Draw the cursor as a one-cell block, and only where someone is typing.

    svg-term's cursor is a grey block a tenth of a cell too wide. fsend never
    hides the cursor, so in a real terminal it also parks at the end of each
    spinner line and at column 0 under the progress bar; in a demo that reads
    as a stray block. Keep it only on rows that hold a shell prompt ("$") or a
    question ("?", the [Y/n] answer), where a viewer expects one.
    """
    m = re.search(r'<symbol id="(\w+)"><path fill="#[0-9a-f]{6}" d="M0 0h1\.102v([\d.]+)H0z"/></symbol>', s)
    assert m, "svg-term output changed: cursor symbol not found"
    sym, row_h = m.group(1), float(m.group(2))
    inset = row_h * 0.08   # a hair shorter than the row, so stacked rows don't touch
    new = (f'<symbol id="{sym}"><rect y="{trim(inset)}" width="1" height="{trim(row_h - 2 * inset)}"'
           f' rx="0.12" fill="{THEMES[theme]["fg"]}" opacity="0.85"/></symbol>')
    s = s.replace(m.group(0), new, 1)

    def text(markup):
        return html.unescape("".join(re.findall(r"<text[^>]*>([^<]*)</text>", markup)))
    rows = {i: text(b) for i, b in re.findall(r'<symbol id="(\w+)">(.*?)</symbol>', s, re.S)}
    use = re.compile(r'<use xlink:href="#%s"(?: x="[-\d.]+")?(?: y="([\d.]+)")?/>' % sym)

    def frame(m):
        body = m.group(2)
        c = use.search(body)
        if not c:
            return m.group(0)
        y = float(c.group(1) or 0)
        # Everything on the cursor's row: symbol rows sit at the row's top,
        # inline runs at its baseline (one font size lower).
        line = "".join(rows.get(i, "") for i, ry in
                       re.findall(r'<use xlink:href="#(\w+)"(?: x="[-\d.]+")? y="([\d.]+)"/>', body)
                       if abs(float(ry) - y) < row_h / 2)
        line += "".join(t for ty, t in re.findall(r'<text[^>]*\by="([\d.]+)"[^>]*>([^<]*)</text>', body)
                        if abs(float(ty) - 1.67 - y) < row_h / 2)
        if "$" in line or "?" in line:
            return m.group(0)
        return m.group(1) + body.replace(c.group(0), "", 1) + m.group(3)
    s, n = re.subn(rf'(<svg width="{COLS}"(?: x="\d+")?>)(.*?)(</svg>)', frame, s, flags=re.S)
    assert n, "svg-term output changed: no filmstrip frames"
    return s


def add_motion(s):
    """Fade the loop's seam, and honour prefers-reduced-motion.

    The fade runs on the terminal content only (the window chrome stays put)
    and shares the filmstrip's duration, so the two stay in lockstep. Viewers
    who ask for reduced motion get the finished transfer as a still: the last
    frame tells the whole story.
    """
    _, dur, final_x = filmstrip(s)
    fin, fout = FADE_IN_S / dur * 100, (dur - FADE_OUT_S) / dur * 100
    css = (f"@keyframes fade{{0%{{opacity:0}}{fin:.4g}%{{opacity:1}}{fout:.4g}%{{opacity:1}}"
           f"100%{{opacity:0}}}}"
           f".fade{{animation:fade {dur:.4g}s linear infinite}}"
           f"@media (prefers-reduced-motion:reduce){{.fade{{animation:none}}"
           f"g[style*=animation-name]{{animation:none!important;"
           f"transform:translateX({final_x}px)}}}}")
    s = re.sub(r"(<style[^>]*>)", lambda m: m.group(1) + css, s, count=1)
    m = re.search(r'<svg height="[\d.]+" viewBox="0 0 \d+ [\d.]+"', s)
    assert m, "svg-term output changed: terminal viewport not found"
    end = s.rindex("</svg>", 0, s.rindex("</svg>"))   # the viewport's own close
    return s[:m.start()] + '<g class="fade">' + s[m.start():end + 6] + "</g>" + s[end + 6:]


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


def _title_bar(w, h, t):
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
        f'<rect x="0" y="0" width="{w:g}" height="{h:g}" rx="{WIN_R}" ry="{WIN_R}" fill="{t["title_bg"]}"/>',
        f'<rect x="0" y="{h - WIN_R:g}" width="{w:g}" height="{WIN_R:g}" fill="{t["title_bg"]}"/>',
        f'<rect x="0" y="{h - 1:g}" width="{w:g}" height="1" fill="{t["hairline"]}"/>',
        f'<text x="{chev_x:g}" y="{base:.2f}" font-family="{FAMILY}" font-size="{f:g}"'
        f' font-weight="700" fill="{CORAL}">&gt;</text>',
        f'<text x="{fs_x:.2f}" y="{base:.2f}" font-family="{FAMILY}" font-size="{f:g}"'
        f' font-weight="500" fill="{t["word_fg"]}">fsend</text>',
        f'<rect x="{cur_x:.2f}" y="{base - 0.10 * f:.2f}" width="{0.46 * f:g}" height="{0.10 * f:g}"'
        f' rx="{0.035 * f:g}" fill="{CORAL}"/>',
    ]
    return "".join(parts)


def add_chrome(s, theme):
    """Title bar, larger traffic lights, and a coral rounded outer frame."""
    h = title_h(s)
    W, _ = (float(x) for x in re.search(
        r'<svg[^>]*\bwidth="([\d.]+)"[^>]*\bheight="([\d.]+)"', s).groups())
    assert 'rx="5" ry="5" class="a"' in s, "svg-term output changed: window bg rect not found"
    s = s.replace('rx="5" ry="5" class="a"', f'rx="{WIN_R}" ry="{WIN_R}" class="a"', 1)
    # title bar sits directly under the window bg, so the dots draw on top of it
    s = re.sub(r'(<rect[^>]*\bclass="a"/>)(<svg y="0%")',
               lambda m: m.group(1) + _title_bar(W, h, THEMES[theme]) + m.group(2), s, count=1)
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
    args = sys.argv[1:]
    theme = "dark"
    if args[:1] == ["--theme"] and len(args) > 1:
        theme, args = args[1], args[2:]
    if len(args) != 2 or theme not in THEMES:
        sys.exit("usage: svg-postprocess.py [--theme dark|light] <in.cast> <out.svg>")
    cast, out = args
    raw = out + ".raw"
    themed = recolor_cast(cast, theme)
    try:
        run_svg_term(themed, raw, theme)
    finally:
        os.remove(themed)
    s = open(raw, encoding="utf-8").read()
    os.remove(raw)
    s = clip_frames(s)
    s = fit_grid(s)
    s = exact_pitch(s)
    s = style_cursor(s, theme)
    s = extend_hold(s)     # after style_cursor, so pruned frames compare equal
    s = add_motion(s)
    s = add_chrome(s, theme)  # before embed_fonts, so the subset covers the chrome text
    s = embed_fonts(s)
    open(out, "w", encoding="utf-8").write(s)
    print(f"wrote {out} ({len(s.encode()) // 1024} KiB)", file=sys.stderr)


if __name__ == "__main__":
    main()
