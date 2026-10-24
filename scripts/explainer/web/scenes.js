/* fsend explainer — the scenes.
 *
 * Every scene is a function (cam, api) -> render(t) where t is seconds since the
 * scene's start (negative while it is cutting in). `cam` is the scene's camera:
 * transform it to push, pan or drift the whole shot. Moments come from the
 * narration via api.on(line, anchor) / api.segStart(line), so a frame is a pure
 * function of t and the visuals track the voice. Sound effects are registered
 * up front with api.cue(type, t) and mixed by sfx.py.
 */
(function () {
  const A = window.Toolkit;
  const { clamp, lerp, ramp, rnd, outQuint, outCubic, outExpo, inCubic, inOutCubic, inOutSine,
          outBack, spring, reveal, hide, el, svg, at, setText, setHTML, words, kin, afterFonts } = A;

  const SPIN = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏";
  const CODE = "abc-defg-jkm";

  /* ------------------------------------------------------------------ *
   *  icons — drawn, never typeset
   * ------------------------------------------------------------------ */
  const LAPTOP = `<svg width="150" height="112" viewBox="0 0 118 88" fill="none" aria-hidden="true">
    <rect x="15" y="5" width="88" height="62" rx="11" stroke="currentColor" stroke-width="3.4" opacity="0.9"/>
    <rect x="23" y="13" width="72" height="46" rx="5" fill="currentColor" opacity="0.07"/>
    <path d="M42 29l10 8.5-10 8.5" stroke="var(--accent-l)" stroke-width="4.4" stroke-linecap="round" stroke-linejoin="round"/>
    <path d="M58 46h13" stroke="var(--accent-l)" stroke-width="4.4" stroke-linecap="round"/>
    <rect x="4" y="73" width="110" height="9" rx="4.5" fill="currentColor" opacity="0.55"/>
  </svg>`;

  const SERVER = `<svg width="100" height="96" viewBox="0 0 92 88" fill="none" aria-hidden="true">
    <rect x="8" y="4" width="76" height="80" rx="12" stroke="currentColor" stroke-width="3.2" opacity="0.9"/>
    ${[0, 1, 2].map((i) => {
      const y = 17 + i * 22;
      return `<rect x="18" y="${y}" width="56" height="16" rx="5" fill="currentColor" opacity="0.09"/>
        <circle cx="27.5" cy="${y + 8}" r="3.6" fill="var(--hubc, var(--accent))"/>
        <rect x="38" y="${y + 6}" width="27" height="4" rx="2" fill="currentColor" opacity="0.25"/>`;
    }).join("")}
  </svg>`;

  const ic = (body, size = 64) =>
    `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor"
      stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${body}</svg>`;
  const LOCK = ic(`<rect x="4.5" y="10.5" width="15" height="10.5" rx="3"/><path d="M8 10.5V7.8a4 4 0 0 1 8 0v2.7"/><circle cx="12" cy="15.7" r="1.3" fill="currentColor"/>`);
  const NOUSER = ic(`<circle cx="12" cy="8" r="3.6"/><path d="M5 20.5c.8-3.6 3.6-5.6 7-5.6s6.2 2 7 5.6"/><path d="M3.5 3.5l17 17"/>`);
  const NOPORT = ic(`<rect x="4" y="6" width="16" height="13" rx="2.5"/><path d="M8 10.5v2.5M11 10.5v2.5M14 10.5v2.5M17 10.5v2.5" opacity="0.8"/><path d="M3 3l18 18"/>`);
  const WALL = ic(`<rect x="3" y="4.5" width="18" height="15" rx="2"/><path d="M3 9.5h18M3 14.5h18M9 4.5v5M15 9.5v5M9 14.5v5"/>`, 70);

  /* the fsend wordmark, outlined from the README logo so it is pixel-identical:
     chevron, the name (revealed letter by letter through a clip), and the cursor */
  let wmId = 0;
  const NAME_PATH = "M111.41 132V96.58H96.88V88.3H111.41V78.09Q111.41 72.02 115.37 68.43Q119.32 64.84 126.13 64.84H140.94V73.03H126.13Q120.52 73.03 120.52 78.18V88.3H140.94V96.58H120.52V132Z M170.94 132.74Q162.29 132.74 157.5 128.87Q152.72 125.01 152.72 118.02H161.74Q161.74 121.33 164.17 123.21Q166.61 125.1 170.94 125.1H174.98Q179.49 125.1 181.93 123.17Q184.37 121.24 184.37 117.74Q184.37 111.58 178.3 110.84L164.68 108.91Q159.25 108.08 156.31 104.45Q153.36 100.81 153.36 95.02Q153.36 88.21 157.96 84.44Q162.56 80.66 170.84 80.66H174.89Q182.62 80.66 187.4 84.34Q192.19 88.02 192.46 94.1H183.26Q183.08 91.52 180.87 89.82Q178.66 88.12 174.89 88.12H170.84Q166.7 88.12 164.45 89.96Q162.2 91.8 162.2 94.92Q162.2 99.98 167.26 100.63L180.04 102.47Q193.2 104.22 193.2 117.74Q193.2 124.92 188.46 128.83Q183.72 132.74 174.98 132.74Z M226.32 132.92Q217.21 132.92 211.65 127.49Q206.08 122.06 206.08 112.68V100.72Q206.08 91.34 211.65 85.91Q217.21 80.48 226.32 80.48Q232.39 80.48 236.95 82.92Q241.5 85.36 244.03 89.73Q246.56 94.1 246.56 99.89V109H215V113.42Q215 118.94 218.04 122.06Q221.08 125.19 226.32 125.19Q230.74 125.19 233.59 123.54Q236.44 121.88 236.99 118.94H246.1Q245.18 125.38 239.75 129.15Q234.32 132.92 226.32 132.92ZM215 99.89V102.28H237.64V99.89Q237.64 94.1 234.69 90.97Q231.75 87.84 226.32 87.84Q220.89 87.84 217.95 90.97Q215 94.1 215 99.89Z M260.18 132V81.4H269.19V91.06H269.28Q269.93 86.09 273.47 83.29Q277.01 80.48 282.81 80.48Q290.35 80.48 294.86 85.26Q299.37 90.05 299.37 98.14V132H290.26V99.62Q290.26 94.1 287.5 91.15Q284.74 88.21 279.96 88.21Q274.99 88.21 272.14 91.29Q269.28 94.37 269.28 99.89V132Z M329.73 132.92Q322.09 132.92 317.58 127.81Q313.08 122.71 313.08 113.97V99.43Q313.08 90.69 317.58 85.59Q322.09 80.48 329.73 80.48Q335.34 80.48 339.07 83.33Q342.79 86.18 343.62 91.15L343.44 79.38V64.84H352.54V132H343.53V122.16Q342.7 127.22 339.02 130.07Q335.34 132.92 329.73 132.92ZM332.86 125.01Q337.73 125.01 340.58 121.97Q343.44 118.94 343.44 113.42V99.98Q343.44 94.46 340.58 91.43Q337.73 88.39 332.86 88.39Q327.8 88.39 324.99 91.01Q322.18 93.64 322.18 99.52V113.88Q322.18 119.76 324.99 122.39Q327.8 125.01 332.86 125.01Z";
  const CLIP_X = [92, 147, 200, 253, 306, 360];      // clip edge after k letters
  const CUR_X = [97, 150, 203, 256, 309, 360];       // cursor x after k letters
  function wordmark(parent, width) {
    const id = "wmclip" + wmId++;
    const n = el("div", "", parent);
    n.style.width = width + "px";
    n.innerHTML = `<svg viewBox="10.5 53.5 409.5 98" width="${width}" height="${(width * 98 / 409.5).toFixed(1)}" fill="none" style="overflow:visible">
      <defs><clipPath id="${id}"><rect x="0" y="40" height="120" width="0"/></clipPath></defs>
      <path class="chev" d="M 37 72 L 64 98.5 L 37 125" stroke="var(--brand)" stroke-width="13" stroke-linecap="round" stroke-linejoin="round" stroke-dasharray="80" stroke-dashoffset="80"/>
      <path clip-path="url(#${id})" d="${NAME_PATH}" fill="#FAF7F5"/>
      <rect class="cur" x="360" y="134" width="40" height="10" rx="3" fill="var(--brand)"/>
    </svg>`;
    const clip = n.querySelector("clipPath rect"), chev = n.querySelector(".chev"), cur = n.querySelector(".cur");
    return {
      node: n,
      // chevron 0..1, typed letters 0..5 (fractional = between keystrokes), cursor alpha
      set(ch, k, curA) {
        chev.setAttribute("stroke-dashoffset", (80 * (1 - outCubic(ch))).toFixed(2));
        const i = Math.floor(clamp(k, 0, 5));
        clip.setAttribute("width", CLIP_X[i]);
        cur.setAttribute("x", CUR_X[i]);
        cur.setAttribute("opacity", curA.toFixed(3));
      },
    };
  }
  const blink = (t, period = 1.0) => (((t % period) + period) % period < period * 0.55 ? 1 : 0);

  /* a soft light, drawn as a gradient (no blur filters) */
  function bloom(parent, x, y, size, color) {
    const b = el("div", "abs", parent);
    Object.assign(b.style, { left: x - size / 2 + "px", top: y - size / 2 + "px", width: size + "px", height: size + "px",
      borderRadius: "50%", background: `radial-gradient(closest-side, ${color}, transparent)` });
    return b;
  }

  /* camera: interpolate between keyframes [t, scale, x, y] with an ease */
  function camera(cam, t, keys, drift = 1) {
    let s = keys[0][1], x = keys[0][2], y = keys[0][3];
    for (let i = 1; i < keys.length; i++) {
      const [t1, s1, x1, y1, d = 0.9] = keys[i];
      const p = inOutCubic(ramp(t, t1, t1 + d));
      s = lerp(s, s1, p); x = lerp(x, x1, p); y = lerp(y, y1, p);
    }
    // handheld-free, but never dead still: a slow breathing drift
    x += drift * 6 * Math.sin(t * 0.45); y += drift * 4 * Math.cos(t * 0.37);
    s *= 1 + 0.004 * drift * Math.sin(t * 0.3);
    cam.style.transform = `translate(${x.toFixed(2)}px, ${y.toFixed(2)}px) scale(${s.toFixed(5)})`;
  }

  /* typing: cue a key click per character */
  function typeCues(api, t0, text, rate, gain = 0.6) {
    for (let i = 0; i < text.length; i++) api.cue("key", t0 + i * rate + (rnd(i * 3.1 + t0) - 0.5) * rate * 0.35, gain);
  }
  const typed = (text, t, t0, rate) => text.slice(0, clamp(Math.floor((t - t0) / rate) + 1, 0, text.length) * (t >= t0 ? 1 : 0));

  /* a terminal window at (x, y) of width w. Returns { node, body } */
  function terminal(parent, x, y, w, h, title) {
    const n = el("div", "term", parent);
    Object.assign(n.style, { left: x + "px", top: y + "px", width: w + "px", height: h + "px" });
    n.innerHTML = `<div class="tb"><i></i><i></i><i></i><div class="ttl">${title}</div></div><div class="body"></div>`;
    return { node: n, body: n.querySelector(".body") };
  }
  const line = (body, cls) => el("div", "ln " + (cls || ""), body);
  const bar = (p, w = 20) => {
    const f = Math.round(clamp(p) * w);
    return `<span class="a">${"━".repeat(f)}</span><span class="f">${"━".repeat(w - f)}</span>`;
  };

  /* ====================================================================== *
   *  OPEN — a cursor types the name; the promise rises underneath
   * ====================================================================== */
  function open(cam, api) {
    const halo = bloom(cam, 960, 470, 1500, "color-mix(in srgb, var(--brand) 34%, transparent)");
    const wmWrap = at(cam, 960, 470, "");
    const wm = wordmark(wmWrap, 1000);
    const l1 = at(cam, 960, 640, "headline");
    const l2 = at(cam, 960, 740, "headline grad");
    const w1 = words(l1, "Private file transfer,");
    const w2 = words(l2, "straight between two computers.");

    const CH0 = 0.5, K0 = 0.92, KR = 0.1, HIT = K0 + 5 * KR + 0.08;
    api.cue("key", CH0 + 0.05, 0.9);
    for (let i = 0; i < 5; i++) api.cue("key", K0 + i * KR, 0.85);
    api.cue("hit", HIT, 1);
    const s2 = api.segStart("open2");

    return function (t) {
      camera(cam, t, [[0, 1.0, 0, 0], [HIT, 1.05, 0, 0, 6]]);
      const k = t < K0 ? 0 : (t - K0) / KR + 1;
      const typing = t > K0 - 0.1 && t < HIT;
      wm.set(ramp(t, CH0, CH0 + 0.3), k, typing ? 1 : blink(t - HIT, 1.06));
      // the punch on the last keystroke, then the lockup lifts to make room
      const punch = t > HIT ? Math.exp(-(t - HIT) * 7) * 0.06 : 0;
      const lift = outQuint(ramp(t, s2 - 0.3, s2 + 0.6));
      wmWrap.style.transform = `translateY(${(-150 * lift).toFixed(1)}px) scale(${(1 + punch - 0.2 * lift).toFixed(4)})`;
      halo.style.opacity = (0.25 + 0.75 * Math.exp(-Math.max(0, t - HIT) * 2.2) * (t > HIT ? 1 : 0) + 0.2 * lift).toFixed(3);
      halo.style.transform = `translateY(${(-150 * lift).toFixed(1)}px)`;
      kin(w1, t, s2 - 0.05, 0.075, 0.6);
      kin(w2, t, api.on("open2", "straight") - 0.15, 0.075, 0.6);
    };
  }

  /* ====================================================================== *
   *  DEMO — one command to send, one to receive
   * ====================================================================== */
  function demo(cam, api) {
    const layer = api.layer;
    cam.style.transformOrigin = "960px 520px";
    // the share code finds its way across first; the file follows the same arc
    const arcSvg = svg("svg", { width: 1920, height: 1080, style: "position:absolute;inset:0;overflow:visible" }, cam);
    const arcPath = svg("path", { d: "", fill: "none", stroke: "var(--brand)", "stroke-width": 3,
      "stroke-linecap": "round", "stroke-dasharray": "2 14", opacity: 0 }, arcSvg);
    // the comet tail behind the flying code (a dash that slides along the arc)
    const trail = svg("path", { d: "", fill: "none", stroke: "var(--brand-l)", "stroke-width": 5,
      "stroke-linecap": "round", pathLength: 1000, opacity: 0 }, arcSvg);
    const dots = [...Array(26)].map(() => svg("circle", { r: 5, fill: "#ffd9c7" }, arcSvg));

    const TW = 860, TH = 470, TY = 262, LX = 70, RX = 990;
    const roleL = at(cam, LX + TW / 2, 226, "role", "<b></b>You");
    const roleR = at(cam, RX + TW / 2, 226, "role", "<b></b>Your friend");
    const S = terminal(cam, LX, TY, TW, TH, "you@laptop — zsh");
    const R = terminal(cam, RX, TY, TW, TH, "friend@desktop — zsh");

    const sCmd = line(S.body);
    line(S.body);
    const sInfo = line(S.body);
    sInfo.innerHTML = `  Sending <b>drone-4k.mov</b>  <span class="d">· 2.3 GB</span>`;
    const box = el("div", "codebox", S.body, `<div class="lbl">On the other machine, run:</div><div class="code">fsend <span class="cv">${CODE}</span></div>`);
    const codeSpan = box.querySelector(".cv");
    const sStat = line(S.body);

    const rCmd = line(R.body);
    line(R.body);
    const rInfo = line(R.body);
    rInfo.innerHTML = `  Incoming <b>drone-4k.mov</b>  <span class="d">· 2.3 GB</span>`;
    const rAsk = line(R.body);
    rAsk.innerHTML = `  <span class="d">from laptop · save to ~/Downloads?</span> <span class="a">Y</span>`;
    line(R.body);
    const rBar = line(R.body);
    const rDone = line(R.body, "ok");
    rDone.textContent = "  ✓ Saved  ~/Downloads/drone-4k.mov";
    const okFlash = bloom(cam, RX + TW / 2, TY + TH / 2, 1100, "color-mix(in srgb, var(--green) 30%, transparent)");

    const fly = at(cam, 0, 0, "flycode", CODE);

    // captions ride outside the camera
    const caps = [["One command to send.", ""], ["One to receive.", ""], ["That’s it.", "grad"]].map(([txt, cls]) => {
      const c = at(layer, 960, 880, "headline " + cls);
      return words(c, txt);
    });

    // geometry, measured once fonts are in (before the camera ever moves)
    let P0 = [480, 560], P2 = [1500, 360];
    afterFonts(() => {
      const r = codeSpan.getBoundingClientRect();
      P0 = [r.left + r.width / 2, r.top + r.height / 2];
      // where the code will sit in the friend's prompt, measured in its own font
      setHTML(rCmd, `<span class="p2">friend@pc</span> <span class="f">~ $</span> fsend <span class="cv">${CODE}</span>`);
      const cr = rCmd.querySelector(".cv").getBoundingClientRect();
      P2 = [cr.left + cr.width / 2, cr.top + cr.height / 2];
      setHTML(rCmd, "");
      const d = `M ${P0[0]} ${P0[1]} Q 960 ${Math.min(P0[1], P2[1]) - 330} ${P2[0]} ${P2[1]}`;
      arcPath.setAttribute("d", d);
      trail.setAttribute("d", d);
    });
    const arcAt = (u) => {
      const c = [960, Math.min(P0[1], P2[1]) - 330];
      const a = (1 - u) * (1 - u), b = 2 * u * (1 - u), d = u * u;
      return [a * P0[0] + b * c[0] + d * P2[0], a * P0[1] + b * c[1] + d * P2[1]];
    };

    // ---- the beats ----
    const CMD = "fsend drone-4k.mov", RATE = 0.052, T0 = 0.35;
    const ENTER = T0 + CMD.length * RATE + 0.12;
    const CODE_IN = api.on("demo1", "code") - 0.25;
    const r0 = api.segStart("demo2");
    const RT0 = r0 + 0.05, RCMD = "fsend ";
    const FLY0 = api.on("demo2", "types") - 0.15, FLY1 = FLY0 + 0.75;
    const RENTER = FLY1 + 0.22;
    const X0 = RENTER + 0.35, X1 = api.on("demo2", "lands") + 0.12;
    const that = api.segStart("demo3");
    typeCues(api, T0, CMD, RATE);
    api.cue("enter", ENTER, 0.9);
    api.cue("pop", CODE_IN + 0.05, 0.9);
    typeCues(api, RT0, RCMD, RATE);
    api.cue("swish", FLY0, 1);
    api.cue("pop", FLY1 - 0.05, 0.8);
    api.cue("enter", RENTER, 0.9);
    api.cue("stream", X0, 1);
    api.cue("chime", X1, 1);

    return function (t) {
      // camera: start on the sender, pan out to both as the friend joins
      camera(cam, t, [[0, 1.16, 530, -10], [r0 - 0.7, 1.0, 0, -10, 1.1], [that - 0.3, 1.05, 0, -20, 1.4]]);

      reveal(roleL, ramp(t, -0.2, 0.3), { dy: 12 });
      // the friend's terminal arrives when the friend is mentioned
      reveal(roleR, ramp(t, r0 - 0.45, r0 + 0.1), { dy: 12 });
      reveal(R.node, ramp(t, r0 - 0.7, r0 + 0.05), { dy: 0, dx: 140, scale: 0.97 });

      // sender
      const sc = typed(CMD, t, T0, RATE);
      setHTML(sCmd, `<span class="p">you@laptop</span> <span class="f">~ $</span> ${sc}${t < ENTER && blink(t, 0.9) ? '<span class="caret"></span>' : ""}`);
      reveal(sInfo, ramp(t, ENTER + 0.1, ENTER + 0.35), { dy: 10 });
      reveal(box, ramp(t, CODE_IN, CODE_IN + 0.45), { dy: 16, scale: 0.94 });
      // the code catches the light when it is named
      const glint = ramp(t, CODE_IN + 0.3, CODE_IN + 1.1);
      codeSpan.style.textShadow = glint > 0 && glint < 1 ? `0 0 ${(28 * Math.sin(Math.PI * glint)).toFixed(1)}px var(--brand)` : "none";
      let stat;
      if (t < X0) stat = `<span class="a">${SPIN[Math.floor(Math.max(0, t) * 12) % 10]}</span> <span class="d">Waiting for receiver…</span>`;
      else if (t < X1) stat = `${bar(ramp(t, X0, X1), 16)} <span class="d">${Math.round(100 * inOutSine(ramp(t, X0, X1)))}%</span>`;
      else stat = `<span class="ok">✓ Sent  2.3 GB</span>`;
      setHTML(sStat, "  " + stat);
      reveal(sStat, ramp(t, CODE_IN + 0.35, CODE_IN + 0.7), { dy: 10 });

      // receiver
      // the code lands highlighted, then settles into plain text
      const hot = t >= FLY1 ? Math.exp(-(t - FLY1) * 2.5) : 0;
      const rc = typed(RCMD, t, RT0, RATE) + (t >= FLY1 ? `<span style="color:color-mix(in srgb, var(--brand-l) ${Math.round(40 + 60 * hot)}%, var(--ink))">${CODE}</span>` : "");
      setHTML(rCmd, `<span class="p2">friend@pc</span> <span class="f">~ $</span> ${rc}${t < RENTER && t > r0 - 0.5 && blink(t, 0.9) ? '<span class="caret"></span>' : ""}`);
      reveal(rInfo, ramp(t, RENTER + 0.1, RENTER + 0.3), { dy: 10 });
      reveal(rAsk, ramp(t, RENTER + 0.2, RENTER + 0.4), { dy: 10 });
      const p = inOutSine(ramp(t, X0, X1));
      const mbps = Math.round(380 + 60 * Math.sin(t * 9) + 30 * Math.sin(t * 23));
      setHTML(rBar, `  ${bar(p, 18)} <span class="d">${String(Math.round(p * 100)).padStart(3)}%  ${t < X1 ? mbps + " MB/s" : ""}</span>`);
      reveal(rBar, ramp(t, X0 - 0.1, X0 + 0.15), { dy: 10 });
      reveal(rDone, ramp(t, X1, X1 + 0.3), { dy: 10, ease: outBack });
      okFlash.style.opacity = (t > X1 ? 0.9 * Math.exp(-(t - X1) * 1.6) : 0).toFixed(3);

      // the flying code: lifts off the sender, arcs over, lands in the prompt
      const u = ramp(t, FLY0, FLY1);
      const e = inOutCubic(u);
      if (u <= 0 || u >= 1) fly.style.opacity = 0;
      else {
        const [x, y] = arcAt(e);
        const sc = lerp(1, 0.64, e) * (1 + 0.3 * Math.sin(Math.PI * e));
        fly.parentElement.style.left = x.toFixed(1) + "px";
        fly.parentElement.style.top = y.toFixed(1) + "px";
        fly.style.transform = `scale(${sc.toFixed(3)}) rotate(${(-5 * Math.sin(Math.PI * 2 * e)).toFixed(2)}deg)`;
        fly.style.opacity = Math.min(1, u * 12, (1 - u) * 14).toFixed(3);
      }
      const tail = 260 * Math.sin(Math.PI * u);
      trail.setAttribute("stroke-dasharray", `${tail.toFixed(1)} 2000`);
      trail.setAttribute("stroke-dashoffset", (tail - 1000 * e).toFixed(1));
      trail.setAttribute("opacity", (u > 0 && u < 1 ? 0.7 : 0).toFixed(2));
      codeSpan.style.opacity = u > 0 && u < 1 ? 0.35 : 1;

      // the file follows the code's arc
      const on = ramp(t, X0 - 0.2, X0) * (1 - ramp(t, X1, X1 + 0.35));
      arcPath.setAttribute("opacity", (0.45 * on).toFixed(3));
      arcPath.setAttribute("stroke-dashoffset", (-(t * 90) % 16).toFixed(2));
      dots.forEach((d, i) => {
        const uu = (i / dots.length + (t - X0) * 1.25) % 1;
        const [x, y] = arcAt(uu);
        d.setAttribute("cx", x.toFixed(1)); d.setAttribute("cy", y.toFixed(1));
        d.setAttribute("opacity", (on * Math.sin(Math.PI * uu) * (0.5 + 0.5 * rnd(i))).toFixed(3));
        d.setAttribute("r", (3 + 4 * Math.sin(Math.PI * uu)).toFixed(2));
      });

      // captions
      kin(caps[0], t, 0.25, 0.07, 0.55, r0 - 0.4);
      kin(caps[1], t, r0 - 0.05, 0.07, 0.55, that - 0.45);
      kin(caps[2], t, that - 0.1, 0.09, 0.6);
    };
  }

  /* ====================================================================== *
   *  TRUST — three promises, slammed in on the words
   * ====================================================================== */
  function trust(cam, api) {
    // a slow drift of ciphertext behind the type
    const rows = [...Array(14)].map((_, r) => {
      const n = el("div", "hexrow", cam);
      n.style.top = 40 + r * 74 + "px";
      let s = "";
      for (let i = 0; i < 90; i++) s += Math.floor(rnd(r * 97 + i) * 65536).toString(16).padStart(4, "0") + " ";
      n.textContent = s;
      return n;
    });
    const items = [
      ["e2e", LOCK, "End-to-end encrypted."],
      ["accounts", NOUSER, "No accounts."],
      ["ports", NOPORT, "No ports to open."],
    ].map(([k, icon, txt], i) => {
      const row = at(cam, 960, 330 + i * 200, "stmt");
      const ib = el("div", "ic", row, icon);
      const tx = el("div", "", row);
      const ws = words(tx, txt);
      const t0 = api.on("trust1", k) - 0.12;
      api.cue("thud", t0 + 0.05, i === 0 ? 1 : 0.85);
      return { row, ib, ws, t0 };
    });
    afterFonts(() => {
      const w = Math.max(...items.map((it) => it.row.getBoundingClientRect().width));
      items.forEach((it) => {
        it.row.parentElement.style.left = (960 - w / 2).toFixed(1) + "px";
        it.row.parentElement.style.transform = "translate(0, -50%)";
      });
    });

    return function (t) {
      camera(cam, t, [[0, 1.0, 0, 0], [0, 1.06, 0, -10, 5]], 1.2);
      rows.forEach((r, i) => {
        const dir = i % 2 ? 1 : -1, sp = 30 + rnd(i) * 40;
        r.style.transform = `translateX(${(-600 + dir * ((t * sp) % 600)).toFixed(1)}px)`;
      });
      items.forEach((it, i) => {
        kin(it.ws, t, it.t0, 0.05, 0.5);
        const p = ramp(t, it.t0 - 0.05, it.t0 + 0.6);
        it.ib.style.transform = `scale(${(p <= 0 ? 0 : spring(p * 1.4, 9, 4.5)).toFixed(4)})`;
        // the newest promise is the brightest; older ones step back, not away
        const next = items[i + 1];
        const back = next ? ramp(t, next.t0, next.t0 + 0.4) : 0;
        it.row.style.opacity = lerp(1, 0.38, back).toFixed(3);
      });
    };
  }

  /* ====================================================================== *
   *  ROUTES — one stage, three answers: local, direct, relay
   * ====================================================================== */
  const GREEN = "#6fdc8c", BLUE = "#7db7ff", ORANGE = "#e0805a";
  function routes(cam, api) {
    const layer = api.layer;
    const AX = 470, BX = 1450, DY = 700, SX = 960, SY = 425;
    const L = { r2: api.segStart("routes2"), r3: api.segStart("routes3"), r4: api.segStart("routes4") };
    api.accent(L.r2 - 0.25, GREEN);
    api.accent(L.r3 - 0.25, BLUE);
    api.accent(L.r4 - 0.25, ORANGE);

    // ---- svg: zones, links, packets ----
    const g = svg("svg", { width: 1920, height: 1080, style: "position:absolute;inset:0;overflow:visible" }, cam);
    const zoneL = svg("rect", { x: 230, y: 582, width: 1460, height: 300, rx: 44, fill: "rgba(255,255,255,0.02)",
      stroke: "var(--accent)", "stroke-opacity": 0.5, "stroke-width": 2.5, "stroke-dasharray": "10 12" }, g);
    const zoneR = svg("rect", { x: 1230, y: 582, width: 460, height: 300, rx: 44, fill: "rgba(255,255,255,0.02)",
      stroke: "var(--accent)", "stroke-opacity": 0.5, "stroke-width": 2.5, "stroke-dasharray": "10 12" }, g);
    const zlab = svg("text", { x: 960, y: 918, class: "zone-label", "text-anchor": "middle" }, g);
    const zlabL = svg("text", { x: 460, y: 918, class: "zone-label", "text-anchor": "middle" }, g);
    const zlabR = svg("text", { x: 1460, y: 918, class: "zone-label", "text-anchor": "middle" }, g);
    zlab.textContent = "same network"; zlabL.textContent = "your network"; zlabR.textContent = "their network";

    const mk = (d, w, color, extra = {}) => svg("path", Object.assign({ d, fill: "none", stroke: color, "stroke-width": w,
      "stroke-linecap": "round", "stroke-linejoin": "round", pathLength: 1000 }, extra), g);
    const DIRECT = `M ${AX + 130} ${DY} L ${BX - 130} ${DY}`;
    const directGlow = mk(DIRECT, 22, "var(--accent)", { opacity: 0 });
    const direct = mk(DIRECT, 5, "var(--accent)");
    const ATT = `M ${AX + 130} ${DY} L ${SX - 70} ${DY}`;
    const attempt = mk(ATT, 5, "var(--red)");
    const SIGA = `M ${AX + 90} ${DY - 105} L ${SX - 115} ${SY + 30}`;
    const SIGB = `M ${BX - 90} ${DY - 105} L ${SX + 115} ${SY + 30}`;
    const sigA = mk(SIGA, 3.5, "var(--accent)", { "stroke-dasharray": "6 12" });
    const sigB = mk(SIGB, 3.5, "var(--accent)", { "stroke-dasharray": "6 12" });
    const RELAY = `M ${AX + 95} ${DY - 100} L ${SX - 105} ${SY + 10} L ${SX + 105} ${SY + 10} L ${BX - 95} ${DY - 100}`;
    const relayGlow = mk(RELAY, 22, "var(--accent)", { opacity: 0 });
    const relay = mk(RELAY, 5, "var(--accent)");
    const pkts = [...Array(7)].map(() => svg("rect", { width: 34, height: 10, rx: 5, x: -17, y: -5, fill: "#fff" }, g));
    const pulses = [...Array(4)].map(() => svg("circle", { r: 9, fill: "var(--accent-l)" }, g));
    const locks = [...Array(6)].map(() => {
      const gg = svg("g", {}, g);
      svg("rect", { x: -22, y: -18, width: 44, height: 36, rx: 9, fill: "var(--accent)" }, gg);
      svg("path", { d: "M-7 -2v-4a7 7 0 0 1 14 0v4", stroke: "#fff", "stroke-width": 3.2, fill: "none", "stroke-linecap": "round" }, gg);
      svg("rect", { x: -9.5, y: -2, width: 19, height: 13, rx: 3, fill: "#fff" }, gg);
      return gg;
    });
    const xmark = svg("g", { opacity: 0 }, g);
    svg("circle", { cx: 0, cy: 0, r: 30, fill: "var(--red)" }, xmark);
    svg("path", { d: "M-10 -10L10 10M10 -10L-10 10", stroke: "#fff", "stroke-width": 5, "stroke-linecap": "round" }, xmark);

    // polyline sampler for the packets
    const pts = (d) => d.match(/-?\d+(\.\d+)?/g).map(Number).reduce((a, v, i, arr) => (i % 2 ? a : [...a, [v, arr[i + 1]]]), []);
    function along(d) {
      const P = pts(d), seg = [];
      let tot = 0;
      for (let i = 1; i < P.length; i++) { const l = Math.hypot(P[i][0] - P[i - 1][0], P[i][1] - P[i - 1][1]); seg.push([P[i - 1], P[i], l, tot]); tot += l; }
      return (u) => {
        let x = clamp(u) * tot;
        for (const [a, b, l, s] of seg) if (x <= s + l) { const f = (x - s) / l; return [lerp(a[0], b[0], f), lerp(a[1], b[1], f)]; }
        return P[P.length - 1];
      };
    }
    const atDirect = along(DIRECT), atRelay = along(RELAY), atA = along(SIGA), atB = along(SIGB);

    // ---- devices + server ----
    const dev = (x, name) => {
      const d = el("div", "dev", cam); d.style.left = x + "px"; d.style.top = DY + "px";
      d.innerHTML = `<div class="card">${LAPTOP}</div><div class="nm">${name}</div>`;
      return d;
    };
    const devA = dev(AX, "your laptop"), devB = dev(BX, "their laptop");
    devA.style.top = devB.style.top = DY + 18 + "px";
    const hub = el("div", "hub", cam);
    hub.style.left = SX + "px"; hub.style.top = SY + "px";
    hub.innerHTML = `<div class="card">${SERVER}</div><div class="nm">fsend server</div>`;
    const wall = at(cam, SX, DY, "", `<div style="width:118px;height:118px;border-radius:30px;display:grid;place-items:center;color:var(--red);
      background:color-mix(in srgb, var(--red) 14%, rgba(10,10,10,0.9));border:2px solid color-mix(in srgb, var(--red) 60%, transparent)">${WALL}</div>`);
    const tagS = at(cam, SX, SY - 107, "tag");
    const tagW = at(cam, SX, DY + 100, "tag");
    tagW.style.setProperty("--c", "var(--red)"); tagW.textContent = "blocked";

    // ---- header (outside the camera) ----
    const intro = at(layer, 960, 170, "headline");
    const introW = words(intro, "It finds the best path. Automatically.");
    const heads = [
      ["01", "Local", "Same network. Straight across.", GREEN],
      ["02", "Direct", "Introduced, then peer to peer.", BLUE],
      ["03", "Relay", "Blocked? Relayed, sealed end to end.", ORANGE],
    ].map(([n, name, sub, c]) => {
      const eb = at(layer, 960, 104, "eyebrow");
      eb.style.color = c;
      const ebw = words(eb, `${n} — ${name}`);
      const h = at(layer, 960, 178, "headline");
      const hw = words(h, sub);
      return { ebw, hw };
    });
    const steps = el("div", "steps", layer);
    const stepEls = [["Local", GREEN], ["Direct", BLUE], ["Relay", ORANGE]].map(([n, c], i) => {
      const s = el("div", "s", steps, `<b>0${i + 1}</b>${n}`);
      return { s, c };
    });

    // ---- moments ----
    const M = {
      across: api.on("routes2", "across"), noserver: api.on("routes2", "noserver"),
      intro: api.on("routes3", "introduces"), steps: api.on("routes3", "steps"),
      blocks: api.on("routes4", "blocks"), relays: api.on("routes4", "relays"), sealed: api.on("routes4", "sealed"),
      path: api.on("routes1", "path"),
    };
    [0.15, 0.3].forEach((d) => api.cue("pop", d + 0.1, 0.7));
    api.cue("pop", 0.5, 0.7);
    api.cue("swish", M.across - 0.3, 0.7);
    api.cue("blip", M.intro - 0.2, 0.8); api.cue("blip", M.intro + 0.35, 0.7);
    api.cue("swish", M.steps - 0.1, 0.7);
    api.cue("deny", M.blocks - 0.05, 1);
    api.cue("swish", M.relays - 0.15, 0.8);
    api.cue("pop", M.sealed, 0.8);
    stepEls.forEach((_, i) => api.cue("tick", M.path + 0.1 + i * 0.12, 0.6));

    const draw = (path, p) => path.setAttribute("stroke-dashoffset", (1000 * (1 - outCubic(p))).toFixed(1));
    [direct, directGlow, attempt, relay, relayGlow].forEach((p) => p.setAttribute("stroke-dasharray", "1000 1000"));

    return function (t) {
      camera(cam, t, [[0, 1.04, 0, -10], [L.r2, 1.1, 0, -30, 1.2], [L.r3, 1.08, 0, -24, 1.2], [L.r4, 1.1, 0, -10, 1.2]]);
      const local = ramp(t, L.r2 - 0.3, L.r2 + 0.3) * (1 - ramp(t, L.r3 - 0.4, L.r3));
      const dPhase = ramp(t, L.r3 - 0.4, L.r3) * (1 - ramp(t, L.r4 - 0.4, L.r4));
      const rPhase = ramp(t, L.r4 - 0.4, L.r4);

      // stage pieces arrive
      reveal(devA, ramp(t, -0.45, 0.2), { dy: 40, scale: 0.9, base: "translate(-50%,-50%)" });
      reveal(devB, ramp(t, -0.35, 0.3), { dy: 40, scale: 0.9, base: "translate(-50%,-50%)" });
      const hubIn = ramp(t, -0.25, 0.4);
      const hubDim = Math.max(local * 0.65, dPhase * 0.55 * ramp(t, M.steps - 0.1, M.steps + 0.5));
      const hubUp = dPhase * ramp(t, M.steps - 0.1, M.steps + 0.6) * -26;
      reveal(hub, hubIn, { dy: 40, scale: 0.9, base: `translate(-50%,-50%) translateY(${hubUp.toFixed(1)}px)`, alpha: 1 - hubDim });
      const hubOn = Math.max(dPhase * ramp(t, M.intro - 0.3, M.intro + 0.2) * (1 - 0.7 * ramp(t, M.steps, M.steps + 0.5)),
                             rPhase * ramp(t, M.relays - 0.3, M.relays + 0.2));
      hub.style.setProperty("--on", hubOn.toFixed(3));

      // header
      kin(introW, t, 0.05, 0.06, 0.55, L.r2 - 0.75);
      const hw = [[L.r2 - 0.35, L.r3 - 0.7], [L.r3 - 0.35, L.r4 - 0.7], [L.r4 - 0.35, null]];
      heads.forEach((h, i) => { kin(h.ebw, t, hw[i][0], 0.05, 0.5, hw[i][1]); kin(h.hw, t, hw[i][0] + 0.08, 0.05, 0.55, hw[i][1]); });
      const active = rPhase > 0.5 ? 2 : dPhase > 0.5 ? 1 : local > 0.5 ? 0 : -1;
      stepEls.forEach(({ s, c }, i) => {
        reveal(s, ramp(t, M.path + 0.1 + i * 0.12, M.path + 0.6 + i * 0.12), { dy: 20 });
        const on = i === active;
        s.style.color = on ? "var(--ink)" : "";
        s.style.borderColor = on ? c : "";
        s.style.background = on ? `color-mix(in srgb, ${c} 16%, transparent)` : "";
        s.style.boxShadow = on ? `0 0 40px -6px ${c}` : "";
        s.querySelector("b").style.color = on ? c : "";
      });

      // zones: one shared network, splitting into two
      const zIn = ramp(t, L.r2 - 0.1, L.r2 + 0.6);
      const split = inOutCubic(ramp(t, L.r3 - 0.2, L.r3 + 0.7));
      zoneL.setAttribute("width", lerp(1460, 460, split).toFixed(1));
      zoneL.setAttribute("opacity", zIn.toFixed(3));
      zoneR.setAttribute("opacity", (split * zIn).toFixed(3));
      zoneR.setAttribute("x", lerp(1430, 1230, split).toFixed(1));
      zoneR.setAttribute("width", lerp(260, 460, split).toFixed(1));
      zlab.setAttribute("opacity", (zIn * (1 - split)).toFixed(3));
      zlabL.setAttribute("opacity", (zIn * split).toFixed(3));
      zlabR.setAttribute("opacity", (zIn * split).toFixed(3));

      // direct link: local (across) and direct (after the server steps aside)
      const dl = Math.max(local * ramp(t, M.across - 0.35, M.across + 0.25), dPhase * ramp(t, M.steps - 0.1, M.steps + 0.45));
      draw(direct, dl); draw(directGlow, dl);
      directGlow.setAttribute("opacity", (0.16 * dl).toFixed(3));
      const devOn = Math.max(dl, rPhase * ramp(t, M.relays, M.relays + 0.4));
      devA.querySelector(".card").style.setProperty("--on", devOn.toFixed(3));
      devB.querySelector(".card").style.setProperty("--on", devOn.toFixed(3));
      pkts.forEach((p, i) => {
        const u = (i / pkts.length + t * 0.55) % 1;
        const [x, y] = atDirect(u);
        p.setAttribute("transform", `translate(${x.toFixed(1)} ${y.toFixed(1)})`);
        p.setAttribute("opacity", (ramp(dl, 0.8, 1) * Math.sin(Math.PI * u)).toFixed(3));
        p.setAttribute("fill", "var(--accent-l)");
      });

      // pairing: the server introduces the two, then steps aside
      const sig = dPhase * ramp(t, M.intro - 0.4, M.intro + 0.1) * (1 - ramp(t, M.steps - 0.1, M.steps + 0.4));
      draw(sigA, 1); draw(sigB, 1);
      sigA.setAttribute("opacity", (0.8 * sig).toFixed(3)); sigB.setAttribute("opacity", (0.8 * sig).toFixed(3));
      sigA.setAttribute("stroke-dashoffset", (-t * 40).toFixed(1)); sigB.setAttribute("stroke-dashoffset", (-t * 40).toFixed(1));
      pulses.forEach((c, i) => {
        const f = i < 2 ? atA : atB, u = ((t - M.intro) * 0.9 + (i % 2) * 0.5) % 1;
        const [x, y] = f(u < 0 ? u + 1 : u);
        c.setAttribute("cx", x.toFixed(1)); c.setAttribute("cy", y.toFixed(1));
        c.setAttribute("opacity", (sig * Math.sin(Math.PI * (u < 0 ? u + 1 : u))).toFixed(3));
      });

      // relay: the direct attempt hits a wall, then the server carries it, sealed
      const att = rPhase * ramp(t, M.blocks - 0.6, M.blocks - 0.05) * (1 - ramp(t, M.relays - 0.2, M.relays + 0.3));
      draw(attempt, rPhase * ramp(t, M.blocks - 0.6, M.blocks - 0.05));
      attempt.setAttribute("opacity", att.toFixed(3));
      const wp = rPhase * ramp(t, M.blocks - 0.12, M.blocks + 0.5);
      const shake = t > M.blocks && t < M.blocks + 0.4 ? Math.sin((t - M.blocks) * 70) * 9 * (1 - (t - M.blocks) / 0.4) : 0;
      wall.style.visibility = wp > 0 ? "visible" : "hidden";
      wall.style.opacity = (Math.min(1, wp * 3) * lerp(1, 0.55, ramp(t, M.relays, M.relays + 0.5))).toFixed(3);
      wall.style.transform = `translateX(${shake.toFixed(1)}px) scale(${spring(wp * 1.3, 9, 5).toFixed(4)})`;
      xmark.setAttribute("opacity", (att > 0.9 ? 1 : 0) * ramp(t, M.blocks - 0.05, M.blocks + 0.1) * (1 - ramp(t, M.relays - 0.2, M.relays + 0.2)));
      xmark.setAttribute("transform", `translate(${SX - 70 - 48 + shake} ${DY - 50})`);
      reveal(tagW, rPhase * ramp(t, M.blocks + 0.05, M.blocks + 0.4), { dy: 10, base: "translate(-50%,-50%)", alpha: 1 - ramp(t, M.relays, M.relays + 0.5) * 0.5 });

      const rl = rPhase * ramp(t, M.relays - 0.3, M.relays + 0.4);
      draw(relay, rl); draw(relayGlow, rl);
      relayGlow.setAttribute("opacity", (0.16 * rl).toFixed(3));
      locks.forEach((k, i) => {
        const u = (i / locks.length + (t - M.relays) * 0.32) % 1;
        const uu = u < 0 ? u + 1 : u;
        const [x, y] = atRelay(uu);
        k.setAttribute("transform", `translate(${x.toFixed(1)} ${y.toFixed(1)})`);
        k.setAttribute("opacity", (ramp(rl, 0.85, 1) * Math.min(1, Math.sin(Math.PI * uu) * 3)).toFixed(3));
      });

      // the server's status tag
      let tag = "", c = "var(--dim)", ta = 0;
      if (local > 0.01) { tag = "not used"; c = "#8a8580"; ta = local * ramp(t, M.noserver - 0.2, M.noserver + 0.2); }
      else if (dPhase > 0.01) {
        const stepped = t > M.steps;
        tag = stepped ? "stepped aside" : "pairing"; c = stepped ? "#8a8580" : BLUE;
        ta = dPhase * ramp(t, M.intro - 0.2, M.intro + 0.2);
      } else if (rPhase > 0.01) {
        const sealed = t > M.sealed - 0.1;
        tag = sealed ? "sees ciphertext only" : "relaying"; c = ORANGE;
        ta = rPhase * ramp(t, M.relays - 0.1, M.relays + 0.3);
      }
      setText(tagS, tag);
      tagS.style.setProperty("--c", c);
      reveal(tagS, ta, { dy: 8, base: `translate(-50%,-50%) translateY(${hubUp.toFixed(1)}px)`, ease: outCubic });
    };
  }

  /* ====================================================================== *
   *  SERVER — free to use, or yours to run
   * ====================================================================== */
  function server(cam, api) {
    const halo = bloom(cam, 1380, 540, 1300, "color-mix(in srgb, var(--brand) 22%, transparent)");
    const eb = el("div", "abs eyebrow", cam); Object.assign(eb.style, { left: "170px", top: "336px" });
    const ebw = words(eb, "The server");
    const h1 = el("div", "abs headline", cam); Object.assign(h1.style, { left: "166px", top: "392px", fontSize: "96px" });
    const h1w = words(h1, "Free to use.");
    const h2 = el("div", "abs headline grad", cam); Object.assign(h2.style, { left: "166px", top: "512px", fontSize: "96px" });
    const h2w = words(h2, "Or run your own.");
    const subl = el("div", "abs sub", cam); Object.assign(subl.style, { left: "170px", top: "650px" });
    subl.innerHTML = `Docker image · <span style="font-family:var(--mono);color:var(--ink)">poliuscorp/fsend</span>`;

    const T = terminal(cam, 1010, 340, 760, 340, "you@vps — zsh");
    const cmd = line(T.body);
    line(T.body);
    const o1 = line(T.body), o2 = line(T.body), o3 = line(T.body);
    o1.innerHTML = `  <span class="ok">✓</span> pairing  <span class="d">:8080/tcp</span>`;
    o2.innerHTML = `  <span class="ok">✓</span> relay    <span class="d">:443/udp</span>`;
    o3.innerHTML = `  <span class="a">●</span> <span class="d">ready</span>`;

    const C = "fsend server", RATE = 0.06;
    const c0 = api.on("server2", "command") - 0.55;
    const enter = c0 + C.length * RATE + 0.1;
    typeCues(api, c0, C, RATE);
    api.cue("enter", enter, 0.9);
    api.cue("chime", enter + 0.45, 0.55);

    return function (t) {
      camera(cam, t, [[0, 1.04, 0, 0], [0, 1.0, 0, 0, 5]]);
      kin(ebw, t, -0.1, 0.06, 0.5);
      kin(h1w, t, 0.1, 0.07, 0.55);
      kin(h2w, t, api.on("server2", "own") - 0.35, 0.07, 0.55);
      reveal(subl, ramp(t, api.on("server2", "command"), api.on("server2", "command") + 0.5), { dy: 14 });
      reveal(T.node, ramp(t, -0.1, 0.6), { dy: 50, scale: 0.96 });
      halo.style.opacity = (0.6 + 0.4 * ramp(t, enter, enter + 0.4)).toFixed(3);
      setHTML(cmd, `<span class="p">you@vps</span> <span class="f">~ $</span> ${typed(C, t, c0, RATE)}${t < enter && blink(t, 0.9) ? '<span class="caret"></span>' : ""}`);
      [o1, o2, o3].forEach((o, i) => reveal(o, ramp(t, enter + 0.15 + i * 0.14, enter + 0.4 + i * 0.14), { dy: 10 }));
    };
  }

  /* ====================================================================== *
   *  OUTRO — the name, one line to install, everywhere it runs
   * ====================================================================== */
  function outro(cam, api) {
    const halo = bloom(cam, 960, 300, 1500, "color-mix(in srgb, var(--brand) 30%, transparent)");
    const wmWrap = at(cam, 960, 300, "");
    const wm = wordmark(wmWrap, 860);
    const tag = at(cam, 960, 470, "sub");
    tag.textContent = "Private file transfer, straight between two computers.";
    const inst = at(cam, 960, 610, "install");
    const IC = "curl -fsSL https://getfsend.alzina.dev | sh";
    const plats = at(cam, 960, 760, "plats");
    const P = [["Linux", "linux"], ["macOS", "macos"], ["Windows", "windows"], ["FreeBSD", "freebsd"]].map(([n, k]) => {
      const p = el("div", "plat", plats, `<svg viewBox="0 0 10 10"><circle cx="5" cy="5" r="3.2" fill="var(--brand)"/></svg>${n}`);
      return { p, t0: api.on("outro2", k) - 0.1 };
    });
    const url = at(cam, 960, 900, "url");
    url.innerHTML = `github.com/polius/fsend<b>·</b>open source`;
    const CUR = (a) => `<span style="display:inline-block;width:0.6em;height:1.05em;vertical-align:-0.16em;background:var(--ink);opacity:${a}"></span>`;
    afterFonts(() => {
      setHTML(inst, `<span class="dl">$</span><span>${IC}${CUR(1)}</span>`);
      inst.style.width = Math.ceil(inst.getBoundingClientRect().width) + "px";
    });

    const HIT = api.segStart("outro1") - 0.12;
    const I0 = api.on("outro2", "install") + 0.15, RATE = 0.028;
    api.cue("hit", HIT, 1);
    typeCues(api, I0, IC, RATE, 0.35);
    P.forEach((p) => api.cue("tick", p.t0 + 0.05, 0.6));

    return function (t) {
      camera(cam, t, [[0, 1.0, 0, 0], [HIT, 1.04, 0, 0, 8]]);
      const pin = ramp(t, HIT - 0.25, HIT + 0.05);
      wm.set(1, 5, t < HIT ? 0 : blink(t - HIT, 1.06));
      const punch = t > HIT ? Math.exp(-(t - HIT) * 6) * 0.05 : 0;
      wmWrap.style.opacity = outQuint(pin).toFixed(3);
      wmWrap.style.transform = `scale(${(lerp(1.35, 1, outExpo(pin)) + punch).toFixed(4)})`;
      halo.style.opacity = (pin * (0.45 + 0.55 * Math.exp(-Math.max(0, t - HIT) * 1.8))).toFixed(3);
      reveal(tag, ramp(t, HIT + 0.35, HIT + 0.95), { dy: 20 });
      reveal(inst, ramp(t, I0 - 0.35, I0 + 0.15), { dy: 26, scale: 0.97 });
      const done = t > I0 + IC.length * RATE;
      setHTML(inst, `<span class="dl">$</span><span>${typed(IC, t, I0, RATE)}${CUR(!done || blink(t, 0.9) ? 0.85 : 0)}</span>`);
      P.forEach(({ p, t0 }) => {
        const q = ramp(t, t0, t0 + 0.45);
        reveal(p, q, { dy: 22, scale: 0.92, ease: outBack });
        p.style.borderColor = `color-mix(in srgb, var(--brand) ${Math.round(60 * Math.exp(-Math.max(0, t - t0) * 1.2) * (q > 0 ? 1 : 0))}%, rgba(255,255,255,0.10))`;
      });
      reveal(url, ramp(t, P[3].t0 + 0.6, P[3].t0 + 1.2), { dy: 14 });
    };
  }

  window.SCENES.open = open;
  window.SCENES.demo = demo;
  window.SCENES.trust = trust;
  window.SCENES.routes = routes;
  window.SCENES.server = server;
  window.SCENES.outro = outro;
})();
