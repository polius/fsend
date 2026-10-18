/* fsend explainer — deterministic render engine.
 *
 * Everything on screen is a pure function of the video timestamp t. The capture
 * harness calls window.__render(t) once per frame and screenshots the page; no
 * wall-clock animation is ever used, so every frame is reproducible.
 *
 * Scenes register into window.SCENES (see scenes.js). The engine positions each
 * scene in its own layer and cross-dissolves between them.
 */
(function () {
  const SCENES = (window.SCENES = {});

  // ---- math / easing ------------------------------------------------------
  const clamp = (x, a = 0, b = 1) => Math.min(b, Math.max(a, x));
  const lerp = (a, b, t) => a + (b - a) * t;
  const easeOutQuint = (t) => 1 - Math.pow(1 - clamp(t), 5);
  const easeOutCubic = (t) => 1 - Math.pow(1 - clamp(t), 3);
  const easeOutExpo = (t) => (t >= 1 ? 1 : 1 - Math.pow(2, -11 * clamp(t)));
  const easeInOutCubic = (t) => { t = clamp(t); return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2; };
  const easeOutBack = (t, s = 1.5) => { t = clamp(t); return 1 + (s + 1) * Math.pow(t - 1, 3) + s * Math.pow(t - 1, 2); };
  const ramp = (t, a, b) => (b <= a ? (t >= b ? 1 : 0) : clamp((t - a) / (b - a)));

  const EASE = { outQuint: easeOutQuint, outCubic: easeOutCubic, outExpo: easeOutExpo,
                 inOutCubic: easeInOutCubic, outBack: easeOutBack, linear: (t) => clamp(t) };

  /* The signature reveal: fade + rise + de-blur (+ optional scale).
     p is raw progress; `ease` shapes it. Returns p for chaining/conditionals. */
  function reveal(node, p, o = {}) {
    if (!node) return p;
    const e = (o.ease || easeOutQuint)(p);
    if (p <= 0) { node.style.opacity = 0; node.style.visibility = "hidden"; return p; }
    node.style.visibility = "visible";
    node.style.opacity = clamp(e);
    const dy = (o.dy ?? 27) * (1 - e);
    const dx = (o.dx ?? 0) * (1 - e);
    const sc = lerp(o.scale ?? 1, 1, e);
    node.style.transform = `translate3d(${dx.toFixed(2)}px, ${dy.toFixed(2)}px, 0) scale(${sc.toFixed(4)})`;
    const bl = (o.blur ?? 13) * (1 - e);
    node.style.filter = bl > 0.25 ? `blur(${bl.toFixed(2)}px)` : "none";
    return p;
  }

  function set(node, p, o = {}) { return reveal(node, p, o); }   // alias, for reading

  // ---- tiny DOM helpers ---------------------------------------------------
  const NS = "http://www.w3.org/2000/svg";
  function el(tag, cls, parent, html) {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (html != null) n.innerHTML = html;
    if (parent) parent.appendChild(n);
    return n;
  }
  function svg(tag, attrs, parent) {
    const n = document.createElementNS(NS, tag);
    for (const k in attrs) n.setAttribute(k, attrs[k]);
    if (parent) parent.appendChild(n);
    return n;
  }
  function text(parent, x, y, str, cls, opts = {}) {
    const t = svg("text", { x, y, class: cls, "text-anchor": opts.anchor || "middle",
                            "dominant-baseline": opts.baseline || "middle" }, parent);
    t.textContent = str;
    return t;
  }
  /* an absolutely-positioned, shrink-wrapped box whose *centre* is (x, y) */
  function box(parent, x, y, cls) {
    const n = document.createElement("div");
    n.className = "pos " + (cls || "");
    n.style.position = "absolute";
    n.style.left = x + "px";
    n.style.top = y + "px";
    n.style.transform = "translate(-50%, -50%)";
    parent.appendChild(n);
    return n;
  }
  function wordmark(parent, size, opts = {}) {
    const n = el("span", "wordmark", parent);
    n.style.fontSize = size + "px";
    n.innerHTML =
      `<span class="chev">&gt;</span><span class="name">fsend</span>` +
      (opts.cursor === false ? "" : `<span class="cur"></span>`);
    const cur = n.querySelector(".cur");
    if (cur) cur.style.cssText = `width:${(size * 0.44).toFixed(1)}px;height:${(size * 0.10).toFixed(1)}px`;
    return { node: n, cursor: cur };
  }

  const A = { clamp, lerp, ramp, reveal, set, el, svg, text, box, wordmark, EASE,
              easeOutQuint, easeOutCubic, easeOutExpo, easeInOutCubic, easeOutBack };
  window.Toolkit = A;

  // ---- timeline -----------------------------------------------------------
  let TL = null;
  function initTimeline(data) {
    const byId = {};
    data.segments.forEach((s) => (byId[s.id] = s));
    TL = { d: data, byId, scenes: data.scenes };
  }

  function api(scene, layer) {
    const t0 = scene.start;
    return Object.assign({
      tl: TL.d, scene, layer, t0, W: 1920, H: 1080,
      dur: scene.end - scene.start,
      rel: (abs) => abs - t0,
      on: (id, label) => TL.byId[id].onsets[label] - t0,
      segStart: (id) => TL.byId[id].start - t0,
      segEnd: (id) => TL.byId[id].end - t0,
      seg: (id) => TL.byId[id],
    }, A);
  }

  // ---- background ---------------------------------------------------------
  const ACCENTS = { brand: "#e0805a", cmd: "#e0805a", modes: "#e0805a", local: "#7ccb8b",
                    direct: "#8fb4de", relay: "#e0805a", cta: "#e0805a", outro: "#e0805a" };
  function hex2rgb(h) { return [parseInt(h.slice(1, 3), 16), parseInt(h.slice(3, 5), 16), parseInt(h.slice(5, 7), 16)]; }

  function paintBackground(t) {
    const bg = document.getElementById("bg");
    // slow drifting aurora
    const a = bg.querySelector(".blob.a"), b = bg.querySelector(".blob.b"), g = bg.querySelector(".blob.c");
    a.style.transform = `translate(${(-260 + 120 * Math.sin(t * 0.11)).toFixed(1)}px, ${(-300 + 90 * Math.cos(t * 0.09)).toFixed(1)}px)`;
    b.style.transform = `translate(${(660 + 150 * Math.sin(t * 0.07 + 1.4)).toFixed(1)}px, ${(160 + 110 * Math.cos(t * 0.08)).toFixed(1)}px)`;
    g.style.transform = `translate(${(720 + 130 * Math.sin(t * 0.06 + 3)).toFixed(1)}px, ${(240 + 100 * Math.sin(t * 0.05 + 2)).toFixed(1)}px)`;
    bg.querySelector(".blob.a").style.opacity = (0.5 + 0.1 * Math.sin(t * 0.13)).toFixed(3);
  }

  function accentRGB(t) {
    const sc = TL.scenes;
    for (let i = 0; i < sc.length; i++) {
      const s = sc[i];
      const c = hex2rgb(ACCENTS[s.name] || "#e0805a");
      if (t < s.end) {
        const h = 0.3;   // half transition window — matches the dip in render()
        if (i > 0 && t < s.start + h) {
          const p = easeInOutCubic(ramp(t, s.start - h, s.start + h));
          const a = hex2rgb(ACCENTS[sc[i - 1].name] || "#e0805a");
          return [lerp(a[0], c[0], p), lerp(a[1], c[1], p), lerp(a[2], c[2], p)];
        }
        return c;
      }
    }
    return hex2rgb("#e0805a");
  }

  // ---- build + render -----------------------------------------------------
  let built = null;   // { scenes: [...], layers: {...} }

  function build() {
    const stage = document.getElementById("stage");
    built = { order: [], layers: {}, renders: {} };
    TL.scenes.forEach((scene, idx) => {
      const layer = el("div", "layer", stage);
      layer.style.zIndex = 10 + idx;
      const r = SCENES[scene.name](layer, api(scene, layer));
      built.order.push(scene);
      built.layers[scene.name] = layer;
      built.renders[scene.name] = r;
    });
  }

  // Transition: the outgoing scene fades away first, then the incoming one fades
  // in — a short dip through the shared background. Scenes pre-compose their
  // *layout* before the boundary (see scenes.js) so the frame that fades in is
  // already whole, and only narration-timed content animates afterwards.
  function render(t) {
    const scenes = built.order;
    let active = 0;
    for (let i = 0; i < scenes.length; i++) if (scenes[i].start <= t) active = i;

    const setAcc = accentRGB(t);
    const root = document.documentElement.style;
    root.setProperty("--accent", `rgb(${setAcc.map(Math.round).join(",")})`);
    root.setProperty("--accent-l", `rgb(${setAcc.map((v) => Math.round(lerp(v, 255, 0.42))).join(",")})`);
    paintBackground(t);

    for (let i = 0; i < scenes.length; i++) {
      const s = scenes[i], layer = built.layers[s.name];
      const outP = i < scenes.length - 1 ? ramp(t, s.end - 0.28, s.end - 0.02) : 0;
      const inP = i > 0 ? ramp(t, s.start - 0.02, s.start + 0.30) : 1;
      const a = (1 - outP) * inP;
      if (a <= 0.004) { layer.style.opacity = 0; layer.style.visibility = "hidden"; continue; }
      layer.style.visibility = "visible";
      layer.style.opacity = a.toFixed(4);
      const sc = lerp(1, 1.025, outP) * lerp(0.985, 1, inP);
      const bl = 5 * outP + 5 * (1 - inP);
      layer.style.transform = `scale(${sc.toFixed(5)})`;
      layer.style.filter = bl > 0.2 ? `blur(${bl.toFixed(2)}px)` : "none";
      built.renders[s.name](t - s.start, { world: t, scene: s, index: i });
    }

    // global fade from/to black
    const bo = document.getElementById("blackout");
    const fadeIn = ramp(t, 0, 0.7);
    const fadeOut = 1 - ramp(t, TL.d.video_end - 1.0, TL.d.video_end - 0.1);
    bo.style.opacity = (1 - Math.min(fadeIn, fadeOut)).toFixed(4);
  }

  // ---- capture API --------------------------------------------------------
  window.__init = function (timelineData) {
    initTimeline(timelineData);
    const bg = document.getElementById("bg");
    el("div", "blob a", bg); el("div", "blob b", bg); el("div", "blob c", bg);
    el("div", "glow-floor", bg); el("div", "vignette", bg);
    const bo = el("div", "", document.getElementById("stage")); bo.id = "blackout";
    bo.style.cssText = "position:absolute;inset:0;background:#000;z-index:999;pointer-events:none";
    build();
    window.__ready = document.fonts.ready.then(() => true);
  };
  window.__render = render;
  window.__duration = () => (TL ? TL.d.video_end : 0);

  // Lay the README play affordance over the current still (see `capture.py
  // --poster`). Not part of the video itself.
  window.__poster = function () {
    const stage = document.getElementById("stage");
    const d = document.createElement("div"); d.id = "poster-play";
    stage.appendChild(d);
  };
})();
