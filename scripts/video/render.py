#!/usr/bin/env python3
"""Render the fsend explainer (1920x1080, 30fps) — audio-driven from timeline.json.

Arc: brand -> one-command demo -> how-it-works overview -> three transfer modes
     (local / direct / relay) -> self-host CTA -> install outro.

Terminal aesthetic: warm-black bg, terracotta #E0805A accent, JetBrains Mono.
"""
import json
import math
import os
import shutil

from PIL import Image, ImageDraw, ImageFont

HERE = os.path.dirname(os.path.abspath(__file__))
W, H = 1920, 1080
OUT = os.path.join(HERE, "frames")
FONT_DIR = os.path.expanduser("~/Library/Fonts")

with open(os.path.join(HERE, "timeline.json")) as f:
    TL = json.load(f)
FPS = TL["fps"]
DUR = TL["video_end"]
SEGMENTS = TL["segments"]
SCENES = TL["scenes"]
# segments grouped by scene, in order
SCENE_SEGS = {}
for s in SEGMENTS:
    SCENE_SEGS.setdefault(s["scene"], []).append(s)

# ---- palette --------------------------------------------------------------
BG       = (18, 14, 11)
PANEL    = (28, 22, 18)
PANEL_HI = (40, 31, 26)
ACCENT   = (224, 128, 90)
ACCENT_L = (240, 168, 138)
TEXT     = (250, 247, 245)
DIM      = (158, 146, 138)
FAINT    = (88, 78, 70)
GREEN    = (124, 203, 139)
RED      = (216, 104, 94)
AMBER    = (226, 176, 102)
BLUE     = (132, 170, 214)
DEVICE   = (188, 178, 171)        # neutral line for laptops/servers (mode color stays on the link)

# ---- fonts ----------------------------------------------------------------
_FC = {}
def _f(name, size):
    k = (name, size)
    if k not in _FC:
        _FC[k] = ImageFont.truetype(os.path.join(FONT_DIR, name), size)
    return _FC[k]
F_BOLD = lambda s: _f("JetBrainsMono-Bold.ttf", s)
F_SEMI = lambda s: _f("JetBrainsMono-SemiBold.ttf", s)
F_MED  = lambda s: _f("JetBrainsMono-Medium.ttf", s)
F_REG  = lambda s: _f("JetBrainsMono-Regular.ttf", s)

# ---- math -----------------------------------------------------------------
def clamp(x, lo=0.0, hi=1.0): return max(lo, min(hi, x))
def lerp(a, b, t): return a + (b - a) * t
def lerp_c(c1, c2, t):
    t = clamp(t)
    return tuple(int(round(lerp(c1[i], c2[i], t))) for i in range(3))
def fade(color, t): return lerp_c(BG, color, clamp(t))
def ease_io(t): t = clamp(t); return t * t * (3 - 2 * t)
def ease_out(t): t = clamp(t); return 1 - (1 - t) ** 3
def seg(t, a, b):
    if b <= a: return 1.0 if t >= b else 0.0
    return clamp((t - a) / (b - a))

# ---- draw helpers ---------------------------------------------------------
def rrect(d, box, r, fill=None, outline=None, width=1):
    d.rounded_rectangle(box, radius=r, fill=fill, outline=outline, width=width)
def tc(d, xy, s, font, fill, anchor="mm", spacing=6):
    d.text(xy, s, font=font, fill=fill, anchor=anchor, spacing=spacing)
def tcc(d, cx, cy, s, font, fill):
    """Like tc(...,'mm') but centers on the glyph INK, not the font metric box —
    so all-caps / no-descender labels are optically centered inside their box."""
    ib = d.textbbox((cx, cy), s, font=font, anchor="mm")
    d.text((cx, 2 * cy - (ib[1] + ib[3]) / 2), s, font=font, fill=fill, anchor="mm")
def tlc(d, x, cy, s, font, fill):
    """Left-anchored, but vertically centered on the glyph ink (optical center)."""
    ib = d.textbbox((x, cy), s, font=font, anchor="lm")
    d.text((x, 2 * cy - (ib[1] + ib[3]) / 2), s, font=font, fill=fill, anchor="lm")
def tw(d, s, font): return d.textlength(s, font=font)

def dashed(d, p0, p1, fill, width=4, dash=18, gap=14, phase=0.0):
    x0, y0 = p0; x1, y1 = p1
    dx, dy = x1 - x0, y1 - y0
    L = math.hypot(dx, dy)
    if L < 1: return
    ux, uy = dx / L, dy / L
    pos = -(phase % (dash + gap))
    while pos < L:
        a = max(0.0, pos); b = min(L, pos + dash)
        if b > a:
            d.line([(x0 + ux * a, y0 + uy * a), (x0 + ux * b, y0 + uy * b)],
                   fill=fill, width=width)
        pos += dash + gap

def glow(d, xy, r, color):
    x, y = xy
    for k, fr in ((2.6, 0.10), (1.7, 0.22), (1.0, 1.0)):
        d.ellipse([x - r * k, y - r * k, x + r * k, y + r * k],
                  fill=lerp_c(BG, color, fr))

def check(d, cx, cy, s, color, wdt=4):
    d.line([(cx - s, cy), (cx - s * 0.25, cy + s * 0.7),
            (cx + s, cy - s * 0.8)], fill=color, width=wdt, joint="curve")

def padlock(d, cx, cy, s, color):
    bw, bh = int(26 * s), int(20 * s)
    r = int(9 * s)
    d.arc([cx - r, cy - bh / 2 - r - int(2 * s), cx + r, cy - bh / 2 + r - int(2 * s)],
          180, 360, fill=color, width=max(3, int(3 * s)))
    rrect(d, [cx - bw / 2, cy - bh / 2, cx + bw / 2, cy + bh / 2], int(5 * s), fill=color)
    d.ellipse([cx - int(3 * s), cy - int(2 * s), cx + int(3 * s), cy + int(5 * s)], fill=BG)

def cloud(d, cx, cy, a):
    col = lerp_c(BG, PANEL_HI, 0.8 * a)
    for dx, dy, r in [(-150, 14, 70), (-66, -26, 92), (44, -30, 96),
                      (150, 10, 74), (0, 40, 120), (-150, 40, 70), (150, 40, 70)]:
        d.ellipse([cx + dx - r, cy + dy - r, cx + dx + r, cy + dy + r], fill=col)

def envelope(d, cx, cy, s, sealed=True):
    w_, h_ = int(34 * s), int(24 * s)
    x0, y0 = cx - w_ // 2, cy - h_ // 2
    rrect(d, [x0, y0, x0 + w_, y0 + h_], int(4 * s), fill=PANEL_HI,
          outline=ACCENT, width=max(2, int(2 * s)))
    d.line([(x0, y0 + 2), (cx, cy + int(2 * s)), (x0 + w_, y0 + 2)],
           fill=ACCENT, width=max(2, int(2 * s)))
    if sealed:
        lx, ly = cx, cy + int(2 * s); r = int(5 * s)
        d.arc([lx - r, ly - r - int(3 * s), lx + r, ly + r - int(3 * s)],
              180, 360, fill=GREEN, width=max(2, int(2 * s)))
        rrect(d, [lx - r, ly - int(2 * s), lx + r, ly + int(7 * s)],
              int(2 * s), fill=GREEN)

# ---- iconography ----------------------------------------------------------
def laptop(d, cx, cy, scale, label, glow_t=0.0):
    """A cleaner laptop: bezelled screen with a faint terminal prompt + hinged base."""
    def I(v): return max(1, int(v * scale))
    sw, sh = I(150), I(96)
    sx0, sy0 = cx - sw // 2, cy - sh // 2 - I(14)
    out = lerp_c(FAINT, DEVICE, glow_t)      # neutral — the mode color lives on the link
    # lid / bezel
    rrect(d, [sx0, sy0, sx0 + sw, sy0 + sh], I(12),
          fill=lerp_c(BG, PANEL_HI, clamp(glow_t * 0.5 + 0.25)),
          outline=out, width=max(2, I(3)))
    # inset screen
    mg = I(11)
    ix0, iy0, ix1, iy1 = sx0 + mg, sy0 + mg, sx0 + sw - mg, sy0 + sh - mg
    rrect(d, [ix0, iy0, ix1, iy1], I(6), fill=lerp_c(BG, PANEL, 0.35 + 0.45 * glow_t))
    # a faint ">_" prompt on the screen, matching the brand wordmark exactly
    pf = _f("JetBrainsMono-Bold.ttf", I(30))
    tc(d, ((ix0 + ix1) // 2, (iy0 + iy1) // 2), ">_", pf,
       lerp_c(lerp_c(BG, PANEL, 0.5), ACCENT_L, clamp(glow_t * 0.7 + 0.15)), anchor="mm")
    # camera dot
    d.ellipse([cx - I(2), sy0 + I(5), cx + I(2), sy0 + I(9)],
              fill=lerp_c(FAINT, DEVICE, glow_t))
    # base with a center hinge notch
    bw = int(sw * 1.36); bx0 = cx - bw // 2; by0 = sy0 + sh + I(4); bh = I(15)
    rrect(d, [bx0, by0, bx0 + bw, by0 + bh], I(7),
          fill=lerp_c(BG, PANEL_HI, clamp(glow_t * 0.5 + 0.3)),
          outline=out, width=max(2, I(2)))
    d.line([(cx - I(20), by0 + bh // 2), (cx + I(20), by0 + bh // 2)],
           fill=lerp_c(PANEL_HI, out, 0.6), width=max(2, I(2)))
    if label:
        tc(d, (cx, by0 + I(50)), label, F_SEMI(I(21)), fade(TEXT, glow_t), anchor="mm")

def server_node(d, cx, cy, scale, active, label):
    """A small rack: three stacked units, each with a status LED + vent slits."""
    def I(v): return max(1, int(v * scale))
    body = lerp_c(PANEL, PANEL_HI, active)
    out = lerp_c(FAINT, DEVICE, active * 0.9)     # neutral chassis; status stays on the LEDs
    txt = lerp_c(FAINT, TEXT, 0.45 + 0.55 * active)
    led = lerp_c(FAINT, GREEN, active)
    w_, h_ = I(150), I(104)
    x0, y0 = cx - w_ // 2, cy - h_ // 2
    rrect(d, [x0, y0, x0 + w_, y0 + h_], I(12), fill=body, outline=out, width=max(2, I(3)))
    unit_h, gap, n = I(25), I(7), 3            # 3 rack units, vertically centered
    block = n * unit_h + (n - 1) * gap
    top = y0 + (h_ - block) // 2
    for i in range(n):
        uy0 = top + i * (unit_h + gap)
        uy1 = uy0 + unit_h
        rrect(d, [x0 + I(11), uy0, x0 + w_ - I(11), uy1], I(4),
              fill=lerp_c(body, BG, 0.32), outline=lerp_c(body, out, 0.45), width=max(1, I(1.5)))
        ucy = (uy0 + uy1) // 2
        glow(d, (x0 + I(24), ucy), max(2, I(3.2)), led)
        for k in range(4):                       # vent slits on the right
            vx = x0 + w_ - I(22) - k * I(9)
            d.line([(vx, uy0 + I(5)), (vx, uy1 - I(5))],
                   fill=lerp_c(body, out, 0.35), width=max(1, I(1.5)))
    tc(d, (cx, y0 + h_ + I(32)), label, F_SEMI(I(26)), txt, anchor="mm")

# ---- window chrome --------------------------------------------------------
WIN = (110, 70, W - 110, H - 70)
TBH = 64
CT = WIN[1] + TBH                      # content top

def base_frame():
    img = Image.new("RGB", (W, H), BG)
    d = ImageDraw.Draw(img)
    rrect(d, list(WIN), 22, fill=BG, outline=lerp_c(BG, FAINT, 0.8), width=2)
    rrect(d, [WIN[0], WIN[1], WIN[2], WIN[1] + TBH], 22, fill=PANEL)
    d.rectangle([WIN[0], WIN[1] + TBH - 22, WIN[2], WIN[1] + TBH], fill=PANEL)
    for i, c in enumerate((RED, AMBER, GREEN)):
        cx = WIN[0] + 34 + i * 30; cy = WIN[1] + TBH // 2
        d.ellipse([cx - 8, cy - 8, cx + 8, cy + 8], fill=c)
    y = WIN[1] + TBH // 2; fx = WIN[0] + 150
    # tight ">"-to-"fsend" kerning, matching the hero wordmark
    aw = tw(d, ">", F_BOLD(30)); nx = fx + aw + 9
    tc(d, (fx, y), ">", F_BOLD(30), ACCENT, anchor="lm")
    tc(d, (nx, y), "fsend", F_MED(30), TEXT, anchor="lm")
    tc(d, (nx + tw(d, "fsend", F_MED(30)) + 3, y), "_", F_BOLD(30), ACCENT, anchor="lm")
    tc(d, (WIN[2] - 34, y), "encrypted peer-to-peer file transfer", F_REG(24),
       DIM, anchor="rm")
    return img, d

# ---- shared widgets -------------------------------------------------------
def chip(d, text, cx, y, a, color=ACCENT, fs=40):
    if a <= 0: return
    a = ease_out(a); font = F_BOLD(fs)
    w_ = tw(d, text, font); pad = 30
    box = [cx - (w_ + pad * 2) / 2, y - fs * 0.9, cx + (w_ + pad * 2) / 2, y + fs * 0.9]
    rrect(d, box, 18, fill=lerp_c(BG, PANEL_HI, a), outline=lerp_c(BG, color, a), width=3)
    tcc(d, cx, y, text, font, fade(TEXT, a))      # optically centered caps

def pill(d, text, cx, cy, a, color=ACCENT, fs=30):
    """A soft rounded tag — used for the brand feature labels."""
    if a <= 0: return
    a = ease_out(a); f = F_MED(fs); w_ = tw(d, text, f); padx = 32; h = fs + 30
    box = [cx - (w_ / 2 + padx), cy - h / 2, cx + (w_ / 2 + padx), cy + h / 2]
    rrect(d, box, h / 2, fill=lerp_c(BG, PANEL, 0.6 * a),
          outline=lerp_c(BG, color, a * 0.85), width=2)
    tc(d, (cx, cy), text, f, fade(TEXT, a), anchor="mm")

def pill_w(d, text, fs=30):
    return tw(d, text, F_MED(fs)) + 64

def mini_term(d, x0, y0, w_, lines, a, title=""):
    a = ease_out(a); lh = 40
    h_ = 34 + lh * len(lines) + (34 if title else 0)
    rrect(d, [x0, y0, x0 + w_, y0 + h_], 12, fill=lerp_c(BG, PANEL, a),
          outline=lerp_c(BG, FAINT, a * 0.9), width=2)
    if title:
        tc(d, (x0 + 22, y0 + 26), title, F_SEMI(24), fade(DIM, a), anchor="lm")
        yy = y0 + 60
    else:
        yy = y0 + 37            # symmetric top/bottom padding when there's no title
    for s, c in lines:
        tc(d, (x0 + 22, yy), s, F_REG(28), fade(c, a), anchor="lm")
        yy += lh
    return h_

def progress_bar(d, cx, y, w_, frac, a):
    h_ = 16; x0 = cx - w_ / 2
    rrect(d, [x0, y - h_/2, x0 + w_, y + h_/2], 8, fill=fade(PANEL_HI, a))
    if frac > 0:
        rrect(d, [x0, y - h_/2, x0 + w_ * clamp(frac), y + h_/2], 8,
              fill=fade(GREEN, a))

# ===========================================================================
# diagram constants — laptops sit centered (both axes) inside their network box
LAP_S = 1.05                     # laptop scale (was 1.4 — too large for the boxes)
MIDY = 747                       # device row: laptop center + connection line
LX, RX = 470, W - 470
SVX, SVY = W // 2, 440
CHIP_Y = 196; SUB_Y = 266; ANNO_Y = 352
# the laptop spans ~MIDY-64 (screen top) to ~MIDY+103 (below its label); pad
# evenly so the laptop+label group is vertically centered inside the enclosure.
NET_Y0, NET_Y1 = MIDY - 104, MIDY + 143
NET_HW = 200                     # half-width — boxes centered on each device

def e2e_note(d, cy, a, msg="End-to-end encrypted"):
    """A small padlock + label — reused across local/direct/relay to show the
    transfer stays end-to-end encrypted on every path. The icon+label group is
    centered on the slide axis (SVX)."""
    if a <= 0: return
    f = F_MED(26); mw = tw(d, msg, f)
    icon_w, gap = 30, 14
    left = SVX - (icon_w + gap + mw) / 2
    padlock(d, left + icon_w / 2, cy, 1.1, fade(GREEN, a))
    tc(d, (left + icon_w + gap + mw / 2, cy), msg, f, fade(GREEN, a), anchor="mm")

def draw_netcontext(d, mode, a):
    """Background context: LAN box (local) vs two networks + internet (direct/relay).
    Each enclosure is centered on its device (laptop + label)."""
    if a <= 0: return
    fillc = lerp_c(BG, PANEL, 0.45 * a)
    bordc = lerp_c(BG, FAINT, 0.7 * a)
    if mode == "local":
        # one LAN enclosure; the chip + subtitle already say "same network"
        rrect(d, [LX - NET_HW, NET_Y0, RX + NET_HW, NET_Y1], 24, fill=fillc,
              outline=bordc, width=2)
    else:
        # the internet (cloud behind the server) between the two private networks;
        # the "over the internet" badge under the chip already names it.
        cloud(d, SVX, SVY, a)
        for cxn, lab in [(LX, "your network"), (RX, "their network")]:
            x0, x1 = cxn - NET_HW, cxn + NET_HW
            rrect(d, [x0, NET_Y0, x1, NET_Y1], 24, fill=fillc,
                  outline=bordc, width=2)
            tc(d, ((x0 + x1) / 2, NET_Y0 - 16), lab, F_SEMI(27),
               fade(DIM, a * 0.9), anchor="mm")

def transfer(d, ctx, mode):
    ts, beat, bt = ctx["ts"], ctx["beat"], ctx["beat_t"]
    appear = ease_out(seg(ts, 0.0, 0.6))

    # per-mode identity color — matches the "How fsend works" cards
    # (local=green, direct=blue, relay=orange) so each mode is visually distinct.
    mc = {"local": GREEN, "direct": BLUE, "relay": ACCENT}[mode]

    # clean header: mode chip (carries the color) + a calm, secondary subtitle
    chip_txt = {"local": "LOCAL", "direct": "DIRECT", "relay": "RELAY"}[mode]
    chip(d, chip_txt, SVX, CHIP_Y, seg(ts, 0.1, 0.7), color=mc)
    sub = {"local": "same network · works offline",
           "direct": "over the internet",
           "relay": "over the internet · when a direct path is blocked"}[mode]
    tc(d, (SVX, SUB_Y), sub, F_MED(30), fade(DIM, seg(ts, 0.4, 1.0)), anchor="mm")

    # network context: LAN enclosure (local) vs two networks + internet (direct/relay)
    draw_netcontext(d, mode, appear)

    sa = {"local": 0.12, "direct": 1.0, "relay": 1.0}[mode]
    laptop(d, LX, MIDY, LAP_S, "your device", appear)
    laptop(d, RX, MIDY, LAP_S, "their device", appear)
    srv_label = {"local": "server  (not contacted)",
                 "direct": "fsend.alzina.dev",
                 "relay": ""}[mode]   # relay address is drawn once, on top of packets
    # local's unused server sits centered between the header and the LAN box,
    # so the gaps above and below it are even
    svy_node = 460 if mode == "local" else SVY
    server_node(d, SVX, svy_node, 1.1, sa * appear, srv_label)

    lp = (LX + 105, MIDY); rp = (RX - 105, MIDY)
    # a small "port" node where the link meets each device — a tidy networked anchor
    for px in (lp, rp):
        d.ellipse([px[0]-7, px[1]-7, px[0]+7, px[1]+7],
                  fill=lerp_c(BG, PANEL_HI, appear), outline=lerp_c(BG, mc, appear*0.8),
                  width=2)
    svl = (SVX - 85, SVY + 38); svr = (SVX + 85, SVY + 38)

    if mode == "local":
        disc = seg(ts, 0.9, 2.1)
        if 0 < disc < 1:
            rr = int(lerp(20, 150, ease_out(disc)))   # centered on your device
            d.ellipse([LX-rr, MIDY-rr, LX+rr, MIDY+rr],
                      outline=lerp_c(BG, mc, 0.45*(1-disc)+0.05), width=3)
        link = ease_out(seg(ts, 2.1, 2.9))
        if link > 0:
            d.line([lp, (lerp(lp[0], rp[0], link), MIDY)], fill=mc, width=6)
        if seg(ts, 2.9, 3.1) > 0:
            flow(d, [lp, rp], ts, 0.5, 3, mc)
            tc(d, (SVX, MIDY+54), "Direct over the LAN", F_SEMI(28),
               fade(mc, seg(ts, 2.9, 3.5)), anchor="mm")
            e2e_note(d, MIDY+114, seg(ts, 3.3, 3.9))   # encrypted even on the LAN

    elif mode == "direct":
        # the server's role label stays on screen for the whole scene
        tc(d, (SVX, ANNO_Y), "Pairing only", F_SEMI(28),
           fade(TEXT, appear), anchor="mm")
        if beat == 0:
            ph = ts * 60
            dashed(d, lp, svl, lerp_c(BG, mc, 0.7), width=4, phase=ph)
            dashed(d, rp, svr, lerp_c(BG, mc, 0.7), width=4, phase=-ph)
            flow(d, [lp, svl], ts, 0.7, 2, mc, size=0.8)
            flow(d, [rp, svr], ts, 0.7, 2, mc, size=0.8)
        else:
            fo = 1 - ease_out(seg(bt, 0.0, 0.5))
            if fo > 0.02:
                dashed(d, lp, svl, lerp_c(BG, mc, 0.7*fo), width=4)
                dashed(d, rp, svr, lerp_c(BG, mc, 0.7*fo), width=4)
            link = ease_out(seg(bt, 0.2, 1.0))
            if link > 0:
                d.line([lp, (lerp(lp[0], rp[0], link), MIDY)], fill=mc, width=6)
            if seg(bt, 1.0, 1.2) > 0:
                flow(d, [lp, rp], ts, 0.5, 3, mc)
                tc(d, (SVX, MIDY+72), "Direct, peer-to-peer",
                   F_SEMI(28), fade(mc, seg(bt, 1.0, 1.6)), anchor="mm")
                e2e_note(d, MIDY+124, seg(bt, 1.4, 2.0))

    elif mode == "relay":
        # the relay's address, drawn once on top (server_node label is empty here)
        tc(d, (SVX, SVY + 120), "fsend.alzina.dev", F_SEMI(29),
           fade(TEXT, appear), anchor="mm")
        fail = seg(ts, 0.7, 1.5)
        if fail > 0:
            mid = ((lp[0]+rp[0])//2, MIDY)
            d.line([lp, mid], fill=lerp_c(BG, RED, 0.7), width=5)
            d.line([rp, mid], fill=lerp_c(BG, RED, 0.7), width=5)
            if seg(ts, 1.2, 1.6) > 0:
                r = 22; aa = seg(ts, 1.2, 1.7); cm = fade(RED, aa)
                d.line([(mid[0]-r, MIDY-r), (mid[0]+r, MIDY+r)], fill=cm, width=6)
                d.line([(mid[0]-r, MIDY+r), (mid[0]+r, MIDY-r)], fill=cm, width=6)
                tc(d, (mid[0], MIDY+60), "direct path blocked",
                   F_MED(26), fade(RED, aa), anchor="mm")
        relay = ease_out(seg(ts, 1.7, 2.5))
        if relay > 0:
            col = lerp_c(BG, mc, relay)
            d.line([lp, svl], fill=col, width=6); d.line([svr, rp], fill=col, width=6)
        if seg(ts, 2.5, 2.7) > 0:
            # two streams — into the relay, then out of it — so no packet is ever
            # drawn on top of the rack itself
            flow(d, [lp, svl], ts, 0.3, 2, mc, sealed=True, size=1.2)
            flow(d, [svr, rp], ts, 0.3, 2, mc, sealed=True, size=1.2)
            e2e_note(d, MIDY+106, seg(ts, 2.8, 3.4))   # even via the relay

def flow(d, points, t, speed, n, color, sealed=False, size=1.0):
    for k in range(n):
        u = (t * speed + k / n) % 1.0
        x, y = packet_pos(points, u)
        if sealed: envelope(d, x, y, size, sealed=True)
        else: glow(d, (x, y), 9 * size, color)

def packet_pos(points, u):
    segs = []; total = 0.0
    for i in range(len(points)-1):
        L = math.hypot(points[i+1][0]-points[i][0], points[i+1][1]-points[i][1])
        segs.append(L); total += L
    if total == 0: return points[0]
    target = clamp(u) * total; acc = 0.0
    for i, L in enumerate(segs):
        if acc + L >= target:
            f = (target-acc)/L if L else 0
            return (lerp(points[i][0], points[i+1][0], f),
                    lerp(points[i][1], points[i+1][1], f))
        acc += L
    return points[-1]

# ===========================================================================
# wordmark
def wordmark(d, cx, cy, fs, a, cursor_blink_t=None, typed=None):
    a = ease_out(a); font = F_BOLD(fs)
    full = "fsend"; shown = full if typed is None else full[:typed]
    gap = fs * 0.30                         # tight gap between ">" and "fsend"
    aw = tw(d, ">", font)
    total_w = aw + gap + tw(d, "fsend", font)
    x0 = cx - total_w / 2
    tc(d, (x0, cy), ">", font, fade(ACCENT, a), anchor="lm")
    fx = x0 + aw + gap
    tc(d, (fx, cy), shown, font, fade(TEXT, a), anchor="lm")
    blink = True if cursor_blink_t is None else (int(cursor_blink_t * 2) % 2 == 0)
    if blink and a > 0.5:
        # underscore cursor, matching the "> fsend_" wordmark
        cxp = fx + tw(d, shown, font) + fs * 0.05
        uw, uh = fs * 0.52, fs * 0.10
        uy = cy + fs * 0.30
        d.rounded_rectangle([cxp, uy, cxp + uw, uy + uh],
                            radius=uh * 0.45, fill=fade(ACCENT, a))

# ---- SCENES ---------------------------------------------------------------
def sc_brand(d, ctx):
    # no "presenting" on screen (the voice still says it); wordmark + what-it-is + pills
    t = ctx["ts"]
    sc = ease_out(seg(t, 0.3, 1.2))
    fs = int(lerp(120, 138, sc))
    wordmark(d, W//2, 452, fs, seg(t, 0.3, 0.9), cursor_blink_t=None)
    # descriptive line — says plainly what fsend is (a CLI tool that sends files)
    tc(d, (W//2, 592), "Send anything, straight from your terminal.",
       F_MED(40), fade(TEXT, seg(t, 0.7, 1.3)), anchor="mm")
    # feature pills — the properties that make it powerful
    pa = seg(t, 1.0, 1.7)
    labels = ["peer-to-peer", "end-to-end encrypted", "open source"]
    gap = 28
    ws = [pill_w(d, x) for x in labels]
    x0 = W//2 - (sum(ws) + gap * (len(labels) - 1)) / 2
    for lab, w in zip(labels, ws):
        pill(d, lab, x0 + w/2, 694, pa)
        x0 += w + gap

def role_badge(d, x, y, text, a, color):
    """A small labeled tag (e.g. Sender / Receiver) above a terminal card."""
    if a <= 0: return
    a = ease_out(a); f = F_SEMI(26); w_ = tw(d, text, f); pad = 22
    rrect(d, [x, y, x + w_ + pad*2, y + 48], 12, fill=lerp_c(BG, PANEL_HI, a),
          outline=lerp_c(BG, color, a*0.8), width=2)
    tcc(d, x + w_/2 + pad, y + 24, text, f, fade(color, a))

def sc_cmd(d, ctx):
    """The whole flow in two terminals: Sender, then Receiver."""
    t, beat, bt = ctx["ts"], ctx["beat"], ctx["beat_t"]
    tc(d, (W//2, 212), "How to use it", F_BOLD(56),
       fade(TEXT, seg(t, 0.0, 0.6)), anchor="mm")
    cw = 1180; x0 = W//2 - cw//2; y0 = 308
    role_badge(d, x0, y0, "Sender", seg(t, 0.0, 0.6), ACCENT)
    slines = [("$ fsend drone-4k.mov", TEXT)]
    if seg(t, 0.8, 1.2) > 0: slines.append(("  drone-4k.mov  ·  2.3 GB", DIM))
    if seg(t, 1.3, 1.7) > 0: slines.append(("  share code:  abc-defg-jkm", ACCENT))
    h1 = mini_term(d, x0, y0 + 62, cw, slines, seg(t, 0.1, 0.7))
    if beat >= 1:
        ry = y0 + 62 + h1 + 56
        role_badge(d, x0, ry, "Receiver", seg(bt, 0.0, 0.5), GREEN)
        frac = ease_io(seg(bt, 0.5, 2.3))
        done = frac >= 0.999
        rlines = [("$ fsend abc-defg-jkm", TEXT)]
        if seg(bt, 0.3, 0.6) > 0:
            rlines.append(("  from your friend · accept? y", DIM))
        a_r = seg(bt, 0.1, 0.7)
        rh = mini_term(d, x0, ry + 62, cw, rlines, a_r)
        py = ry + 62 + rh + 48
        progress_bar(d, W//2, py, cw - 40, frac, a_r)
        lbl = ("✓ saved drone-4k.mov" if done
               else f"receiving…  {int(frac*100)}%   ·   112 MB/s")
        tc(d, (W//2, py + 44), lbl, F_MED(30),
           fade(GREEN if done else DIM, a_r), anchor="mm")
        if done:
            tc(d, (W//2, py + 128), "That's all. Simple and secure.",
               F_SEMI(32), fade(TEXT, seg(bt, 2.4, 3.0)), anchor="mm")

def sc_modes(d, ctx):
    """Overview: fsend tries the most private path first, falling back as needed."""
    t = ctx["ts"]
    tc(d, (W//2, 232), "How fsend works", F_BOLD(56),
       fade(TEXT, seg(t, 0.0, 0.6)), anchor="mm")
    cards = [
        ("LOCAL",  "same network → connect directly",       "no server · works offline",   GREEN),
        ("DIRECT", "across the internet → connect directly", "server only introduces them", BLUE),
        ("RELAY",  "across the internet → relayed",          "always end-to-end encrypted", ACCENT),
    ]
    conds = ["if they're not on the same network", "if a strict network blocks it"]
    # appear in step with the narration ("local… directly… falls back to a relay")
    appear_t = [3.6, 6.8, 11.2]
    cw, ch = 1180, 122; x0 = W//2 - cw//2; y0 = 300; step = ch + 94
    for i, (name, desc, note, col) in enumerate(cards):
        cy = y0 + i*step
        a = ease_out(seg(t, appear_t[i], appear_t[i] + 0.7))
        if a <= 0: continue
        rrect(d, [x0, cy, x0+cw, cy+ch], 18, fill=fade(PANEL, a),
              outline=lerp_c(BG, col, a*0.55), width=2)
        # optically centered on the desc/note block center (cy+ch//2+2)
        tlc(d, x0+46, cy+ch//2+2, name, F_BOLD(40), fade(col, a))
        d.line([(x0+254, cy+28),(x0+254, cy+ch-28)], fill=lerp_c(BG, FAINT, a*0.7), width=2)
        tc(d, (x0+296, cy+ch//2-20), desc, F_MED(32), fade(TEXT, a), anchor="lm")
        tc(d, (x0+296, cy+ch//2+24), note, F_REG(26), fade(DIM, a), anchor="lm")
        # fallback arrow + condition into the next card
        if i < len(cards)-1:
            aa = ease_out(seg(t, appear_t[i+1] - 1.4, appear_t[i+1] - 0.7))
            if aa > 0:
                ax = x0 + 90; ya = cy+ch+12; yb = cy+step-10
                d.line([(ax, ya), (ax, yb)], fill=fade(FAINT, aa), width=3)
                d.line([(ax-12, yb-16),(ax, yb),(ax+12, yb-16)],
                       fill=fade(FAINT, aa), width=3, joint="curve")
                tc(d, (ax+34, (ya+yb)//2), conds[i], F_REG(26),
                   fade(DIM, aa), anchor="lm")

def sc_cta(d, ctx):
    """Self-hosting is first-class: run your own relay in one command.
    (Install is covered in the outro, not here.)"""
    t, beat, bt = ctx["ts"], ctx["beat"], ctx["beat_t"]
    a0 = seg(t, 0.0, 0.7)
    tc(d, (W//2, 262), "Want full control? Run your own.",
       F_BOLD(56), fade(TEXT, a0), anchor="mm")
    tc(d, (W//2, 332), "the public server is free — no setup needed.",
       F_MED(26), fade(DIM, seg(t, 0.3, 1.0)), anchor="mm")

    # beat1: a terminal showing `fsend server`, then where to find Docker + the docs
    if beat >= 1:
        a1 = ease_out(seg(bt, 0.0, 0.7))
        cw = 880; x0 = W//2 - cw//2; ty = 455
        lines = [("$ fsend server", TEXT)]
        if seg(bt, 0.5, 0.9) > 0:
            lines.append(("→ listening on :443", GREEN))
        th = mini_term(d, x0, ty, cw, lines, a1)
        # Docker image, documented (not run inline) — heading + ref stacked,
        # mirroring the self-host guide block below.
        da = seg(bt, 1.0, 1.6)
        if da > 0:
            tc(d, (W//2, ty + th + 74), "Docker image",
               F_SEMI(29), fade(TEXT, da), anchor="mm")
            tc(d, (W//2, ty + th + 116), "poliuscorp/fsend",
               F_MED(26), fade(ACCENT_L, da), anchor="mm")
        # the self-hosting guide — a labeled link, not a bare URL
        la = seg(bt, 1.4, 2.0)
        if la > 0:
            tc(d, (W//2, ty + th + 204), "How to self-host the server",
               F_SEMI(29), fade(TEXT, la), anchor="mm")
            tc(d, (W//2, ty + th + 246),
               "github.com/polius/fsend/blob/main/docs/self-hosting.md",
               F_MED(26), fade(ACCENT_L, la), anchor="mm")

def install_row(d, cy, os_label, cmd, a, w=1300):
    """One install line: an OS label, a divider, then the command to run."""
    if a <= 0: return
    a = ease_out(a); x0 = W//2 - w//2; h = 70
    rrect(d, [x0, cy-h/2, x0+w, cy+h/2], 14, fill=lerp_c(BG, PANEL, 0.7*a),
          outline=lerp_c(BG, FAINT, a*0.8), width=2)
    tc(d, (x0+34, cy), os_label, F_SEMI(27), fade(DIM, a), anchor="lm")
    dx = x0 + 440          # label column sized to fit the longest OS label
    d.line([(dx, cy-22), (dx, cy+22)], fill=lerp_c(BG, FAINT, a*0.6), width=2)
    tc(d, (dx+30, cy), cmd, F_SEMI(29), fade(ACCENT_L, a), anchor="lm")

def sc_outro(d, ctx):
    # wordmark + "How to install" heading + three install rows, then the closing
    # line lands (synced to VO).
    t, beat, bt = ctx["ts"], ctx["beat"], ctx["beat_t"]
    wordmark(d, W//2, 310, 150, seg(t, 0.2, 0.9), cursor_blink_t=None)
    tc(d, (W//2, 455), "How to install", F_SEMI(34),
       fade(DIM, seg(t, 0.4, 1.0)), anchor="mm")
    install_row(d, 547, "Linux · macOS · FreeBSD",
                "curl -fsSL https://getfsend.alzina.dev | sh", seg(t, 0.6, 1.2))
    install_row(d, 635, "macOS (Homebrew)",
                "brew install polius/tap/fsend", seg(t, 0.7, 1.3))
    install_row(d, 723, "Windows",
                "irm https://getfsend.alzina.dev/windows | iex", seg(t, 0.9, 1.5))
    tc(d, (W//2, 958), "github.com/polius/fsend", F_MED(26),
       fade(DIM, seg(t, 1.1, 1.7)), anchor="mm")
    # the closing line, on screen, timed to the spoken "What you send stays…"
    # closing punch — three pills, each popping in as its word is spoken.
    # onsets are fractions of the spoken line's length, so they track the voice
    # (and adapt if the TTS engine paces it differently).
    if beat >= 1:
        dur = ctx["seg"]["dur"]
        # measured onsets of "Simple, / private / and free" as fractions of the line
        words = ["Simple", "Private", "Free"]; fracs = [0.0, 0.39, 0.77]
        gap = 26; ws = [pill_w(d, w, 34) for w in words]
        x0 = W//2 - (sum(ws) + gap * (len(words) - 1)) / 2
        for w, pw, fr in zip(words, ws, fracs):
            at = fr * dur
            pill(d, w, x0 + pw/2, 843, ease_out(seg(bt, at, at + 0.2)), fs=34)
            x0 += pw + gap

DRAW = {"brand": sc_brand,
        "cmd": sc_cmd, "modes": sc_modes,
        "local": lambda d, c: transfer(d, c, "local"),
        "direct": lambda d, c: transfer(d, c, "direct"),
        "relay": lambda d, c: transfer(d, c, "relay"),
        "cta": sc_cta, "outro": sc_outro}

# ---- context + frame ------------------------------------------------------
def scene_ctx(scene, t):
    segs = SCENE_SEGS[scene["name"]]
    cur = segs[0]
    for s in segs:
        if s["start"] - 0.12 <= t: cur = s
        else: break
    starts = [s["start"] - scene["start"] for s in segs]
    return {"t": t, "ts": t - scene["start"], "sd": scene["end"] - scene["start"],
            "beat": cur["beat"], "beat_t": t - cur["start"],
            "seg": cur,
            "beat_starts": starts,
            "scene_end_rel": scene["end"] - scene["start"]}

def scene_at(t):
    for sc in SCENES:
        if sc["start"] <= t < sc["end"]: return sc
    return SCENES[-1]

def frame_for(scene, t):
    img, d = base_frame()
    DRAW[scene["name"]](d, scene_ctx(scene, t))
    return img

DISS = 0.42
CHROME = base_frame()[0]            # bare terminal window (identical on every scene)
def render_frame(t):
    sc = scene_at(t)
    idx = SCENES.index(sc)
    img = frame_for(sc, t)
    # Dip the CONTENT through the bare chrome at each scene boundary. Because the
    # window/title bar is identical across scenes, only the content fades in/out —
    # so two scenes' text never cross-fade on top of each other (no ghosting).
    if idx > 0 and t < sc["start"] + DISS/2:
        img = Image.blend(CHROME, img, clamp((t - sc["start"]) / (DISS/2)))
    elif idx < len(SCENES)-1 and t > sc["end"] - DISS/2:
        img = Image.blend(CHROME, img, clamp((sc["end"] - t) / (DISS/2)))
    # global fades through black at the very open and close
    if t < 0.4:
        img = Image.blend(img, Image.new("RGB", (W, H), BG), 1 - t/0.4)
    if t > DUR - 0.6:
        img = Image.blend(img, Image.new("RGB", (W, H), BG), clamp((t-(DUR-0.6))/0.6))
    return img

def main():
    # wipe stale frames first — a shorter render must not leave trailing frames
    # from a previous longer one, or ffmpeg's globbing pads the video past the audio
    if os.path.isdir(OUT):
        shutil.rmtree(OUT)
    os.makedirs(OUT, exist_ok=True)
    total = int(round(DUR * FPS))
    for i in range(total):
        render_frame(i / FPS).save(os.path.join(OUT, f"f{i:05d}.png"))
        if i % 60 == 0:
            print(f"  frame {i}/{total}  (t={i/FPS:.1f}s)", flush=True)
    print(f"done: {total} frames")

if __name__ == "__main__":
    main()
