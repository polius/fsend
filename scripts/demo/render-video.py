#!/usr/bin/env python3
"""Render the animated demo.svg to raster demo.mp4 / demo.gif.

For sites that don't render SVG, this re-bakes the *existing* animated SVG into
raster video without re-recording. Headless Chrome renders the SVG (native CSS +
woff2, pixel-identical to a browser) and we drive its animation deterministically
via the Web Animations API: pause it, then seek to evenly spaced times and
screenshot each. Constant cadence keeps the text rock-steady (a real-time
screencast resampled to a fixed fps makes the edges shimmer), and a 2x device
scale supersamples the small terminal text so it stays crisp after encoding.

Output is an H.264 mp4 (small, the format to share) and an optimized gif.

Usage: scripts/demo/render-video.py [docs/assets/demo.svg]
Outputs alongside the SVG: demo.mp4 (yuv420p, even dims) and demo.gif.
Requires: Google Chrome, ffmpeg, and the `websocket-client` python package.
"""
import base64, bisect, json, os, re, shutil, subprocess, sys, tempfile, time
from urllib.request import urlopen
import websocket  # websocket-client (synchronous)

SRC = sys.argv[1] if len(sys.argv) > 1 else "docs/assets/demo.svg"
OUT_DIR = os.path.dirname(SRC) or "."
BASE = os.path.splitext(os.path.basename(SRC))[0]
MP4 = os.path.join(OUT_DIR, BASE + ".mp4")
GIF = os.path.join(OUT_DIR, BASE + ".gif")
FPS = 30
SCALE = 2          # device scale: supersample the small text, then encode crisp
GIF_FPS = 15
CHROME = "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

svg = open(SRC).read()
m = re.search(r'<svg[^>]*\bwidth="([0-9.]+)"[^>]*\bheight="([0-9.]+)"', svg)
W, H = (round(float(m.group(1))), round(float(m.group(2)))) if m else (1079, 707)
dur_m = re.search(r'animation-duration:([0-9.]+)s', svg)
LOOP = float(dur_m.group(1)) if dur_m else 27.3

# @keyframes p stops -> the times at which the frame actually changes; we only
# screenshot one image per distinct stop and reuse it for the held samples.
ks = svg.index('@keyframes p{') + len('@keyframes p{')
depth, i = 1, ks
while depth:
    depth += {'{': 1, '}': -1}.get(svg[i], 0)
    i += 1
pcts = sorted(float(p) for p in
              re.findall(r'([0-9.]+)%\{transform:translateX\(', svg[ks:i-1]))
if not pcts or pcts[0] > 0:
    pcts.insert(0, 0.0)
stop_ms = [p / 100.0 * LOOP * 1000 for p in pcts]
print(f"{W}x{H} @ {SCALE}x, {LOOP}s loop, {len(stop_ms)} stops", file=sys.stderr)

tmp = tempfile.mkdtemp(prefix="fsend-demo-")
chrome = None
try:
    html = os.path.join(tmp, "demo.html")
    open(html, "w").write(
        "<!doctype html><meta charset=utf-8>"
        "<style>html,body{margin:0;background:#1e1e2e}"
        f"svg{{display:block;width:{W}px;height:{H}px}}</style>" + svg)

    profile = os.path.join(tmp, "profile")
    chrome = subprocess.Popen(
        [CHROME, "--headless=new", "--remote-debugging-port=0",
         "--remote-allow-origins=*", f"--user-data-dir={profile}",
         f"--window-size={W},{H}", "--hide-scrollbars", "--no-first-run",
         "--no-default-browser-check", "--disable-gpu", "file://" + html],
        stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)

    portfile = os.path.join(profile, "DevToolsActivePort")
    port = None
    for _ in range(100):
        if os.path.exists(portfile):
            port = open(portfile).read().splitlines()[0]
            break
        time.sleep(0.1)
    if not port:
        sys.exit("Chrome did not expose a DevTools port")

    target = next(t for t in json.load(urlopen(f"http://127.0.0.1:{port}/json"))
                  if t.get("type") == "page")
    ws = websocket.create_connection(target["webSocketDebuggerUrl"],
                                     max_size=None, timeout=30)
    seq = 0
    def call(method, **params):
        global seq
        seq += 1
        ws.send(json.dumps({"id": seq, "method": method, "params": params}))
        while True:                       # skip events, return the matching reply
            msg = json.loads(ws.recv())
            if msg.get("id") == seq:
                if "error" in msg:
                    raise RuntimeError(f"{method}: {msg['error']}")
                return msg.get("result", {})

    def js(expr):
        r = call("Runtime.evaluate", expression=expr, returnByValue=True)
        return r.get("result", {}).get("value")

    call("Page.enable")
    call("Runtime.enable")
    # Exact viewport at 2x so the screenshot is 2W x 2H of crisp text.
    call("Emulation.setDeviceMetricsOverride", width=W, height=H,
         deviceScaleFactor=SCALE, mobile=False)
    for _ in range(50):
        if js("document.readyState") == "complete":
            break
        time.sleep(0.1)
    # Grab the SVG's CSS animation and pause it so we can scrub frame-exactly.
    ok = js('(()=>{const el=document.querySelector(\'g[style*="animation-name:p"]\');'
            'if(!el)return "noel";const a=el.getAnimations()[0];'
            'if(!a)return "noanim";a.pause();window.__a=a;return "ok";})()')
    if ok != "ok":
        sys.exit(f"could not hook the SVG animation: {ok}")

    # Sample at constant fps; screenshot once per distinct stop, reuse for holds.
    total = round(LOOP * FPS)
    shot, frame_files = {}, []
    for n in range(total):
        t_ms = n * 1000.0 / FPS
        idx = max(bisect.bisect_right(stop_ms, t_ms) - 1, 0)
        if idx not in shot:
            js(f"window.__a.currentTime={t_ms}")
            data = call("Page.captureScreenshot", format="png",
                        captureBeyondViewport=False)["data"]
            fp = os.path.join(tmp, f"s{idx:05d}.png")
            open(fp, "wb").write(base64.b64decode(data))
            shot[idx] = fp
        frame_files.append(shot[idx])
        print(f"\r  frame {n+1}/{total} ({len(shot)} unique)", end="", file=sys.stderr)
    print(file=sys.stderr)
    ws.close()

    # Constant-duration concat -> steady playback, no resampling jitter.
    concat = os.path.join(tmp, "concat.txt")
    with open(concat, "w") as f:
        for fp in frame_files:
            f.write(f"file '{fp}'\nduration {1.0/FPS:.5f}\n")
        f.write(f"file '{frame_files[-1]}'\n")

    even = "scale=trunc(iw/2)*2:trunc(ih/2)*2"
    print("encoding mp4...", file=sys.stderr)
    subprocess.run(["ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", concat,
                    "-vf", even, "-r", str(FPS), "-c:v", "libx264", "-crf", "18",
                    "-preset", "slow", "-pix_fmt", "yuv420p",
                    "-movflags", "+faststart", MP4],
                   check=True, stdin=subprocess.DEVNULL)

    print("encoding gif...", file=sys.stderr)
    pal = os.path.join(tmp, "pal.png")
    # Keep the gif at the full 2x capture resolution — any downscale averages
    # the supersampled text and reintroduces blur, so we match the mp4's detail.
    gif_vf = f"fps={GIF_FPS}"
    subprocess.run(["ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", concat,
                    "-vf", f"{gif_vf},palettegen=stats_mode=full", pal],
                   check=True, stdin=subprocess.DEVNULL)
    subprocess.run(["ffmpeg", "-y", "-f", "concat", "-safe", "0", "-i", concat,
                    "-i", pal, "-lavfi",
                    f"{gif_vf}[x];[x][1:v]paletteuse=dither=none",
                    GIF], check=True, stdin=subprocess.DEVNULL)
finally:
    if chrome:
        chrome.terminate()
    shutil.rmtree(tmp, ignore_errors=True)

for p in (MP4, GIF):
    print(f"wrote {p} ({os.path.getsize(p)/1e6:.1f} MB)", file=sys.stderr)
