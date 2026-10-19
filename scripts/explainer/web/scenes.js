/* fsend explainer — the scenes.
 *
 * Every scene is a function (layer, api) -> render(t) where t is seconds since
 * the scene started. Nothing is animated by CSS: render(t) computes the state of
 * every moving part from the timeline onsets, so a frame is a pure function of t.
 *
 * Layout is measured, not guessed. Each screen is a single top-down stack placed
 * by *top edge*, so a box that grows (a terminal gaining a line) pushes what
 * follows instead of colliding with it, and every screen shares one vertical
 * rhythm. Measurements run once, after the real fonts are in place.
 */
(function () {
  const A = window.Toolkit;
  const { el, svg, text, box, reveal, ramp, lerp, clamp, afterFonts } = A;

  // writing text only when it changed: a per-frame textContent write would
  // rebuild the text node on every captured frame for nothing
  const setText = (n, s) => { if (n.textContent !== s) n.textContent = s; };

  /* ------------------------------------------------------------------ *
   *  placement
   * ------------------------------------------------------------------ */

  // an absolutely-placed, centre-anchored element whose *own* transform is free
  function anchor(layer, x, y, cls) {
    const inner = el("div", "rv" + (cls ? " " + cls : ""), box(layer, x, y));
    return inner;
  }
  // centre-x, top-anchored: the element's top edge lands on `top`
  function atTop(layer, x, top, cls) {
    const outer = box(layer, x, top);
    outer.style.transform = "translate(-50%, 0)";
    // absolute boxes shrink to whatever room is left of `left`, which would
    // wrap a headline at 960px. Size to the content instead and centre it.
    outer.style.width = "max-content";
    return el("div", "rv" + (cls ? " " + cls : ""), outer);
  }
  // left-anchored (badges that line up with a card's left edge), top-anchored
  function atTopLeft(layer, x, top, cls) {
    const outer = box(layer, x, top);
    outer.style.transform = "translate(0, 0)";
    return el("div", "rv" + (cls ? " " + cls : ""), outer);
  }
  const hOf = (node) => node.parentElement.getBoundingClientRect().height;
  function moveTo(node, y) { node.parentElement.style.top = y.toFixed(1) + "px"; }
  // place a run of boxes top-down. rows = [node, gapAfterTheNode]
  function stack(top, rows) {
    let y = top;
    for (const [node, gapAfter] of rows) {
      if (!node) continue;
      moveTo(node, y);
      y += hOf(node) + gapAfter;
    }
    return y;
  }
  // a row of shrink-wrapped pills, centred on x=960 at `y`. Measured once:
  // absolutely-positioned boxes would otherwise report a zero width.
  function rowOf(nodes, y, gap = 22, cx = 960) {
    afterFonts(() => {
      const w = nodes.map((n) => n.getBoundingClientRect().width);
      const total = w.reduce((a, b) => a + b, 0) + gap * (nodes.length - 1);
      let x = cx - total / 2;
      nodes.forEach((n, i) => {
        n.parentElement.style.left = (x + w[i] / 2).toFixed(1) + "px";
        n.parentElement.style.top = y + "px";
        x += w[i] + gap;
      });
    });
    return nodes;
  }

  /* ------------------------------------------------------------------ *
   *  geometry: a tiny polyline sampler
   *  (used instead of getPointAtLength so the capture loop never touches
   *  the layout engine for motion)
   * ------------------------------------------------------------------ */
  function sampler(d) {
    const n = d.match(/-?\d+(?:\.\d+)?/g).map(Number);
    const pts = [];
    for (let i = 0; i + 1 < n.length; i += 2) pts.push([n[i], n[i + 1]]);
    const segs = [];
    let total = 0;
    for (let i = 1; i < pts.length; i++) {
      const dx = pts[i][0] - pts[i - 1][0], dy = pts[i][1] - pts[i - 1][1];
      const l = Math.sqrt(dx * dx + dy * dy);
      segs.push({ a: pts[i - 1], b: pts[i], l, s: total });
      total += l;
    }
    return {
      length: total,
      at(u) {
        let x = clamp(u) * total;
        for (const g of segs) {
          if (x <= g.l || g === segs[segs.length - 1]) {
            const f = g.l ? clamp(x / g.l) : 0;
            return [lerp(g.a[0], g.b[0], f), lerp(g.a[1], g.b[1], f)];
          }
          x -= g.l;
        }
        return pts[pts.length - 1];
      },
    };
  }

  /* ------------------------------------------------------------------ *
   *  icons — all drawn, never typeset, so they never depend on metrics
   * ------------------------------------------------------------------ */

  const LAPTOP = `<svg width="118" height="88" viewBox="0 0 118 88" fill="none" aria-hidden="true">
    <rect x="15" y="5" width="88" height="62" rx="11" stroke="currentColor" stroke-width="3.4" opacity="0.85"/>
    <rect x="23" y="13" width="72" height="46" rx="5" fill="currentColor" opacity="0.07"/>
    <path d="M42 29l10 8.5-10 8.5" stroke="var(--accent-l)" stroke-width="4.4"
      stroke-linecap="round" stroke-linejoin="round"/>
    <path d="M58 46h13" stroke="var(--accent-l)" stroke-width="4.4" stroke-linecap="round"/>
    <rect x="4" y="73" width="110" height="9" rx="4.5" fill="currentColor" opacity="0.5"/>
  </svg>`;

  const SERVER = `<svg width="92" height="88" viewBox="0 0 92 88" fill="none" aria-hidden="true">
    <rect x="8" y="4" width="76" height="80" rx="12" stroke="currentColor" stroke-width="3.2" opacity="0.85"/>
    ${[0, 1, 2].map((i) => {
      const y = 17 + i * 22;
      return `<rect x="18" y="${y}" width="56" height="16" rx="5" fill="currentColor" opacity="0.09"/>
        <circle cx="27.5" cy="${y + 8}" r="3.6" fill="var(--accent)"/>
        <rect x="38" y="${y + 6}" width="27" height="4" rx="2" fill="currentColor" opacity="0.22"/>`;
    }).join("")}
  </svg>`;

  const LOCK = `<svg viewBox="0 0 24 24" width="23" height="23" fill="none" aria-hidden="true">
    <rect x="4.6" y="10.4" width="14.8" height="10.2" rx="3.2" stroke="currentColor" stroke-width="2.2"/>
    <path d="M7.9 10.4V7.9a4.1 4.1 0 0 1 8.2 0v2.5" stroke="currentColor"
      stroke-width="2.2" stroke-linecap="round"/>
    <circle cx="12" cy="15.5" r="1.7" fill="currentColor"/></svg>`;

  const CONTAINER = `<svg viewBox="0 0 28 26" width="31" height="29" fill="none" aria-hidden="true">
    <path d="M14 3.2l11 6.2v8.4L14 24 3 17.8V9.4z" stroke="currentColor" stroke-width="1.9"
      stroke-linejoin="round"/>
    <path d="M3 9.4l11 6.2 11-6.2M14 15.6V24" stroke="currentColor" stroke-width="1.9"
      stroke-linejoin="round" opacity="0.65"/></svg>`;

  const DOC = `<svg viewBox="0 0 26 26" width="28" height="28" fill="none" aria-hidden="true">
    <rect x="4.6" y="2.6" width="16.8" height="20.8" rx="3.4" stroke="currentColor"
      stroke-width="1.9"/>
    <path d="M9 9h8M9 13.2h8M9 17.4h5" stroke="currentColor" stroke-width="1.9"
      stroke-linecap="round"/></svg>`;

  // -- icons for the evidence row under each diagram ------------------------
  const BOLT = `<svg viewBox="0 0 24 24" width="25" height="25" fill="none" aria-hidden="true">
    <path d="M13.5 2.5 5.9 13.6h5.2l-1 7.9 7.9-11.1h-5.2z" stroke="currentColor"
      stroke-width="1.9" stroke-linejoin="round"/></svg>`;

  const STEP_ASIDE = `<svg viewBox="0 0 29 24" width="29" height="24" fill="none" aria-hidden="true">
    <rect x="2.6" y="3.4" width="12.4" height="17.2" rx="2.9" stroke="currentColor" stroke-width="1.9"/>
    <path d="M6.5 8.7h4.6M6.5 12.4h4.6" stroke="currentColor" stroke-width="1.7"
      stroke-linecap="round" opacity="0.55"/>
    <path d="M19 12h6.6M22.4 8.6 25.8 12l-3.4 3.4" stroke="currentColor" stroke-width="1.9"
      stroke-linecap="round" stroke-linejoin="round"/></svg>`;

  const SHIELD = `<svg viewBox="0 0 22 24" width="23" height="24" fill="none" aria-hidden="true">
    <path d="M11 2.4 20 5.8v6.4c0 4.4-3.4 7.6-9 9.2-5.6-1.6-9-4.8-9-9.2V5.8z" stroke="currentColor"
      stroke-width="1.9" stroke-linejoin="round"/>
    <path d="M7.2 12 10 14.8l5-5.6" stroke="currentColor" stroke-width="1.9"
      stroke-linecap="round" stroke-linejoin="round"/></svg>`;

  // the three routes, drawn as a family so they can be read side by side.
  // each carries a `mflow` packet that travels the live path from frame one.
  function modeGlyph(kind) {
    const dev = (x) => `
      <rect x="${x}" y="78" width="58" height="42" rx="9" stroke="currentColor" stroke-width="3.2" opacity="0.85"/>
      <rect x="${x + 8}" y="86" width="42" height="26" rx="4" fill="currentColor" opacity="0.08"/>
      <rect x="${x + 12}" y="124" width="34" height="4" rx="2" fill="currentColor" opacity="0.3"/>`;
    const srv = (extra) => `<rect x="102" y="18" width="36" height="30" rx="9"
      stroke="currentColor" stroke-width="3.2" ${extra || ""}/>`;
    const packet = (color) =>
      `<circle class="mflowh" cx="68" cy="99" r="15" fill="${color}" opacity="0"/>
       <circle class="mflow" cx="68" cy="99" r="7" fill="${color}" opacity="0"/>`;
    const head = `<svg width="420" height="245" viewBox="0 0 240 140" fill="none" aria-hidden="true">`;
    const devices = dev(10) + dev(172);
    if (kind === "local") {
      return head + devices +
        `<g class="mtrace">
           <path d="M68 99h104" stroke="var(--green)" stroke-width="6" stroke-linecap="round"/>
           <circle cx="120" cy="99" r="9" fill="var(--green)"/>
         </g>` + packet("var(--green)") + `</svg>`;
    }
    if (kind === "direct") {
      return head + `<g class="mtrace" stroke="currentColor" stroke-width="3" fill="none"
        opacity="0.35">${srv()}</g>` + devices +
        `<g class="mtrace">
           <path d="M68 99L120 48M172 99L120 48" stroke="currentColor" stroke-width="2.4"
             stroke-dasharray="9 9" opacity="0.32"/>
           <path d="M68 99h104" stroke="var(--blue)" stroke-width="6" stroke-linecap="round"/>
         </g>` + packet("var(--blue)") + `</svg>`;
    }
    return head + `<g class="mtrace"><g fill="#17120f">${srv("stroke=\"none\"")}${srv()}</g></g>` + devices +
      `<g class="mtrace">
         <path d="M68 99h104" stroke="currentColor" stroke-width="2.6" stroke-dasharray="2 11"
           stroke-linecap="round" opacity="0.35"/>
         <path d="M68 99L120 48M172 99L120 48" stroke="var(--accent)" stroke-width="6"
           stroke-linecap="round"/>
       </g>` + packet("var(--accent)") + `</svg>`;
  }

  // where the `mflow` packet sits at u in [0,1) for each route
  const MODE_FLOW = {
    local: (u) => [lerp(68, 172, u), 99],
    direct: (u) => [lerp(68, 172, u), 99],
    relay: (u) => (u < 0.5
      ? [lerp(68, 120, u * 2), lerp(99, 48, u * 2)]
      : [lerp(120, 172, (u - 0.5) * 2), lerp(48, 99, (u - 0.5) * 2)]),
  };

  /* ------------------------------------------------------------------ *
   *  cards
   * ------------------------------------------------------------------ */

  function deviceCard(layer, x, y, name) {
    const card = anchor(layer, x, y, "card dev");
    card.innerHTML = `<div class="glyph">${LAPTOP}</div><div class="name">${name}</div>`;
    return card;
  }
  function hubCard(layer, x, y, name) {
    const card = anchor(layer, x, y, "card hub");
    card.innerHTML = `<div class="glyph">${SERVER}</div><div class="name">${name}</div>
      <div class="tag"></div>`;
    return card;
  }

  // a terminal window. lines are either typed out character by character
  // (`type: true`) or simply revealed.
  function makeTerm(layer, cx, width, name, lines) {
    const c = atTop(layer, cx, 0, "term");
    c.style.width = width + "px";
    c.innerHTML = `<div class="bar"><i class="r"></i><i class="y"></i><i class="g"></i>
      <span class="name">${name}</span></div><div class="body"></div>`;
    const body = c.querySelector(".body");
    const rows = lines.map((l) => {
      const n = el("div", "ln" + (l.cls ? " " + l.cls : ""), body);
      n.style.visibility = "hidden";
      if (l.type) {
        const tx = el("span", "tx", n);
        const cur = el("span", "caret", n);
        return { node: n, text: l.text, type: true, tx, cur };
      }
      if (l.html != null) n.innerHTML = l.html; else n.textContent = l.text;
      return { node: n, text: l.text, type: false };
    });
    return { card: c, rows };
  }
  // type `text` into a row, p = 0..1
  function typeInto(row, p) {
    const shown = Math.round(clamp(p) * row.text.length);
    row.tx.textContent = row.text.slice(0, shown);
    row.cur.style.opacity = shown >= row.text.length ? 0 : 1;
  }

  // the accent tick belongs under the *word*, not under the whole lockup
  // (the chevron hangs to the left and would drag the centre off)
  function alignUnderline(wm, under) {
    afterFonts(() => {
      const name = wm.querySelector(".name");
      const r = name.getBoundingClientRect();
      under.parentElement.style.left = (r.left + r.width / 2).toFixed(1) + "px";
    });
  }

  /* SVG links ------------------------------------------------------------ */
  function dashLink(parent, d, color, width) {
    return svg("path", { d, fill: "none", stroke: color, "stroke-width": width,
      "stroke-dasharray": "14 16", "stroke-linecap": "round" }, parent);
  }
  function solidLink(parent, d, color, width) {
    // a wide, low-opacity stroke reads as a glow without an SVG filter (which
    // the rasterizer would have to re-run for every captured frame)
    const glow = svg("path", { d, fill: "none", stroke: color, "stroke-width": width * 3.6,
      "stroke-linecap": "round" }, parent);
    const core = svg("path", { d, fill: "none", stroke: color, "stroke-width": width,
      "stroke-linecap": "round" }, parent);
    return { glow, core };
  }
  // the draw-on reveal: a dash pattern of one full length, pulled open
  function drawOn(link, p) {
    const L = link.core.dataset.len;
    link.core.style.opacity = p;
    link.glow.style.opacity = 0.30 * p;
    if (p > 0) {
      link.core.setAttribute("stroke-dasharray", `${L * p} ${L}`);
      link.glow.setAttribute("stroke-dasharray", `${L * p} ${L}`);
    }
  }

  /* flowing packets — a head with a fading comet trail -------------------- */
  function makeFlow(parent, smp, opts) {
    const { n = 3, r = 9, color = "#fff", sealed = false, size = 1 } = opts;
    const items = [];
    for (let i = 0; i < n; i++) {
      const g = svg("g", {}, parent);
      const trails = [];
      if (!sealed) {
        for (let j = 0; j < 3; j++) {
          trails.push(svg("circle",
            { r: r * (1 - j * 0.24), fill: color, opacity: (0.30 - j * 0.08).toFixed(2) }, g));
        }
        svg("circle", { r: r * 1.95, fill: color, opacity: 0.16 }, g);
        svg("circle", { r, fill: color }, g);
      } else {
        const w = 34 * size, h = 24 * size;
        svg("circle", { r: r * 2.4, fill: color, opacity: 0.14 }, g);
        svg("rect", { x: -w / 2, y: -h / 2, width: w, height: h, rx: 4 * size,
          fill: "#120e0c", stroke: color, "stroke-width": 2.2 }, g);
        svg("path", { d: `M ${-w / 2} ${-h / 2 + 2} L 0 ${2 * size} L ${w / 2} ${-h / 2 + 2}`,
          fill: "none", stroke: color, "stroke-width": 2.2 }, g);
        svg("circle", { cx: 0, cy: 2 * size, r: 3.1 * size, fill: color }, g);
      }
      items.push({ node: g, trails });
      g.style.opacity = 0;   // nothing shows until update() runs
    }
    return {
      update(t, speed, alpha) {
        if (alpha <= 0.001) { items.forEach((it) => (it.node.style.opacity = 0)); return; }
        items.forEach((it, i) => {
          const u = ((t * speed + i / items.length) % 1 + 1) % 1;
          const [px, py] = smp.at(u);
          it.node.setAttribute("transform",
            `translate(${px.toFixed(2)},${py.toFixed(2)})`);
          // fade in/out at the ends for a clean entry/exit
          const edge = Math.min(1, Math.min(u, 1 - u) / 0.12);
          it.node.style.opacity = (alpha * clamp(edge)).toFixed(3);
          it.trails.forEach((c, j) => {
            const [tx, ty] = smp.at(u - (j + 1) * 0.026);
            c.setAttribute("cx", (tx - px).toFixed(2));
            c.setAttribute("cy", (ty - py).toFixed(2));
          });
        });
      },
      hide() { items.forEach((it) => (it.node.style.opacity = 0)); },
    };
  }

  /* a soft cloud behind the hub (gradient, so no blur filter is needed) --- */
  function cloud(parent, cx, cy, alpha) {
    const g = svg("g", { opacity: 0 }, parent);
    [[0, 6, 300, 168], [-168, 26, 152, 112], [176, 26, 152, 112], [0, 58, 214, 108]]
      .forEach(([dx, dy, rx, ry]) =>
        svg("ellipse", { cx: cx + dx, cy: cy + dy, rx, ry, fill: "url(#cloudgrad)" }, g));
    return { set(a) { g.setAttribute("opacity", (0.85 * clamp(a)).toFixed(3)); } };
  }

  /* ====================================================================== *
   *  SCENE: brand
   * ====================================================================== */
  function brand(layer, api) {
    const wm = anchor(layer, 960, 440);
    const w = A.wordmark(wm, 150);
    const under = anchor(layer, 960, 534);
    under.innerHTML = `<div class="underline"></div>`;
    alignUnderline(wm, under);
    const tag = anchor(layer, 960, 604);
    tag.className += " lead";
    tag.style.textAlign = "center";
    tag.textContent = "Send any file to any device, straight from your terminal.";

    const pills = ["peer-to-peer", "end-to-end encrypted", "open source"].map((lab) => {
      const p = anchor(layer, 0, 706, "pill");
      p.textContent = lab;
      return p;
    });
    rowOf(pills, 706, 24);

    return function (t) {
      reveal(wm, ramp(t, 0.7, 1.55));
      if (w.cursor) w.cursor.style.opacity = Math.floor(t * 2) % 2 === 0 ? 1 : 0.05;
      const up = A.easeOutExpo(ramp(t, 0.92, 1.95));
      under.querySelector(".underline").style.transform = `scaleX(${up.toFixed(3)})`;
      reveal(tag, ramp(t, 2.45, 3.35));
      pills.forEach((p, i) => reveal(p, ramp(t, 3.5 + i * 0.16, 4.15 + i * 0.16),
        { ease: A.easeOutBack, dy: 16, scale: 0.9, blur: 6 }));
    };
  }

  /* ====================================================================== *
   *  SCENE: cmd — the two terminals
   * ====================================================================== */
  function cmd(layer, api) {
    const CW = 1080;
    const LEFT = 960 - CW / 2;

    const head = atTop(layer, 960, 0, "h2");
    head.textContent = "One command to send, one to receive.";

    const sBadge = atTopLeft(layer, LEFT, 0, "badge");
    sBadge.textContent = "Sender";
    const sender = makeTerm(layer, 960, CW, "you — zsh", [
      { text: "$ fsend drone-4k.mov", type: true },
      { text: "  drone-4k.mov   ·   2.3 GB", cls: "dim" },
      { html: `  share code:  <span class="code">abc-defg-jkm</span>`, text: "", cls: "acc" },
    ]);

    const rBadge = atTopLeft(layer, LEFT, 0, "badge ok");
    rBadge.textContent = "Receiver";
    const recv = makeTerm(layer, 960, CW, "your friend — zsh", [
      { text: "$ fsend abc-defg-jkm", type: true },
      { text: "  from your friend   ·   accept? y", cls: "dim" },
    ]);

    const bar = atTop(layer, 960, 0);
    bar.style.width = CW + "px";
    bar.innerHTML = `<div class="bar-track"><div class="bar-fill"></div></div>`;
    const fill = bar.querySelector(".bar-fill");
    const status = atTop(layer, 960, 0, "mono");
    status.style.fontSize = "28px";

    afterFonts(() => {
      // A section label belongs to the card *below* it: 14px under the label
      // starts the card, 58px above the label separates it from the card above.
      stack(96, [
        [head, 46],
        [sBadge, 14],
        [sender.card, 58],
        [rBadge, 14],
        [recv.card, 46],
        [bar, 26],
        [status, 0],
      ]);
    });

    return function (t) {
      reveal(head, ramp(t, -0.6, -0.05));
      reveal(sBadge, ramp(t, -0.55, -0.05));
      reveal(sender.card, ramp(t, -0.5, 0.0));
      // the command types itself, then its output lands
      const tp = ramp(t, 0.05, 0.95);
      typeInto(sender.rows[0], tp);
      reveal(sender.rows[0].node, ramp(t, 0.0, 0.2), { dy: 12, blur: 6 });
      reveal(sender.rows[1].node, ramp(t, 1.15, 1.65), { dy: 12, blur: 6 });
      reveal(sender.rows[2].node,
        ramp(t, api.on("cmd1", "code") - 0.2, api.on("cmd1", "code") + 0.35), { dy: 12, blur: 6 });

      const r0 = api.segStart("cmd2");
      reveal(rBadge, ramp(t, r0, r0 + 0.55));
      reveal(recv.card, ramp(t, r0 + 0.08, r0 + 0.85));
      typeInto(recv.rows[0], ramp(t, r0 + 0.4, r0 + 1.15));
      reveal(recv.rows[0].node, ramp(t, r0 + 0.3, r0 + 0.6), { dy: 12, blur: 6 });
      reveal(recv.rows[1].node,
        ramp(t, api.on("cmd2", "runs") - 0.25, api.on("cmd2", "runs") + 0.2), { dy: 12, blur: 6 });

      const p0 = api.on("cmd2", "runs") + 0.2, p1 = api.on("cmd2", "lands") + 0.35;
      const p = A.easeInOutCubic(ramp(t, p0, p1));
      reveal(bar, ramp(t, p0 - 0.15, p0 + 0.25));
      fill.style.width = (p * 100).toFixed(2) + "%";
      const done = t >= p1;
      status.style.color = done ? "var(--green)" : "var(--dim)";
      setText(status, done ? "✓ saved  drone-4k.mov   ·   112 MB/s"
                           : `receiving…  ${Math.round(p * 100)}%   ·   112 MB/s`);
      reveal(status, ramp(t, p0, p0 + 0.3));
    };
  }

  /* ====================================================================== *
   *  SCENE: modes — the three routes, side by side, lit as they are named
   * ====================================================================== */
  const MODES = [
    { kind: "local",  name: "LOCAL",  title: "Same network",
      note: "both devices on one network — no server in the way",
      color: "var(--green)",  anchor: "local" },
    { kind: "direct", name: "DIRECT", title: "Over the internet",
      note: "a direct link — the server only introduces them",
      color: "var(--blue)",   anchor: "direct" },
    { kind: "relay",  name: "RELAY",  title: "Strict networks",
      note: "the server carries the file when direct is blocked",
      color: "var(--accent)", anchor: "relay" },
  ];

  function modes(layer, api) {
    const head = atTop(layer, 960, 0, "h2");
    head.textContent = "How fsend works";
    const sub = atTop(layer, 960, 0, "lead");
    sub.style.fontSize = "31px";
    sub.textContent = "fsend picks the best path on its own — nothing to configure.";

    const CW = 500, CGAP = 46;
    const x0 = 960 - (CW * 3 + CGAP * 2) / 2 + CW / 2;
    const cards = MODES.map((d, i) => {
      const card = atTop(layer, x0 + i * (CW + CGAP), 0, "modecard");
      card.style.width = CW + "px";
      card.style.setProperty("--mc", d.color);
      card.innerHTML =
        `<div class="msweep"></div>
         <div class="mglyph">${modeGlyph(d.kind)}</div>
         <div class="mlabel"><span class="mdot"></span>${d.name}</div>
         <div class="mname">${d.title}</div>
         <div class="mnote">${d.note}</div>
         <div class="mbar"></div>`;
      return { node: card, packet: card.querySelector(".mflow"),
               halo: card.querySelector(".mflowh"), def: d };
    });

    afterFonts(() => {
      moveTo(head, 96);
      moveTo(sub, 96 + hOf(head) + 30);
      const top = 96 + hOf(head) + 30 + hOf(sub) + 74;
      cards.forEach((c) => moveTo(c.node, top));
    });

    return function (t) {
      reveal(head, ramp(t, -0.62, -0.06));
      reveal(sub, ramp(t, -0.56, 0.04));
      cards.forEach((c, i) => {
        const appear = -0.50 - i * 0.13;
        reveal(c.node, ramp(t, appear, appear + 0.62), { dy: 24, scale: 0.98 });
        // the card the narration is naming owns the glow, the sheen and a
        // faster packet — everything else stays exactly as readable as before
        const at = api.on("modes" + (i + 2), c.def.anchor);
        const on = A.easeOutCubic(ramp(t, at - 0.05, at + 0.45));
        c.node.style.setProperty("--on", on.toFixed(3));
        // the packet runs at a constant speed: the highlight changes the glow,
        // never the phase, or the dot would jump back every time a card lights
        const u = ((t * 0.30) % 1 + 1) % 1;
        const [fx, fy] = MODE_FLOW[c.def.kind](u);
        const edge = clamp(Math.min(u, 1 - u) / 0.10) * (0.58 + 0.42 * on);
        c.packet.setAttribute("cx", fx.toFixed(1));
        c.packet.setAttribute("cy", fy.toFixed(1));
        c.packet.setAttribute("opacity", edge.toFixed(3));
        c.halo.setAttribute("cx", fx.toFixed(1));
        c.halo.setAttribute("cy", fy.toFixed(1));
        c.halo.setAttribute("opacity", (0.19 * edge).toFixed(3));
      });
    };
  }

  /* ====================================================================== *
   *  the transfer diagrams (local / direct / relay share one stage)
   *
   *  One stage, three stories: your device on the left, their device on the
   *  right, one wire between them, and a single slot in the middle that holds
   *  whatever that route needs — nothing (local), the pairing server (direct)
   *  or the relay (relay). Same geometry every time, so the three slides read
   *  as three answers to the same question.
   * ====================================================================== */
  const LXC = 420, RXC = 1500;            // device centres
  const DYC = 744;                        // the wire runs along their mid-line
  const LX = 580, RX = 1340;              // where the wire meets each device
  const NODE_CY = 451;                    // the middle slot
  const BOX_Y0 = 608, BOX_Y1 = 880;       // network enclosures
  const STATE_Y = 814;                    // the live caption, under the wire
  const FACT_Y = 962;                     // the evidence row

  const MODE_CFG = {
    local: {
      title: "Direct over your own network",
      sub: "both devices are on the same network",
      state: "direct over the LAN",
      fact: { icon: BOLT, text: "works offline" },
    },
    direct: {
      title: "Straight across the internet",
      sub: "the server only introduces them",
      state: "direct, peer to peer",
      fact: { icon: STEP_ASIDE, text: "no relay in the middle" },
    },
    relay: {
      title: "Relayed through a server",
      sub: "it only ever sees sealed, encrypted packets",
      state: "relayed · packets stay sealed",
      fact: { icon: SHIELD, text: "works when blocked" },
    },
  };

  function transferScene(layer, api, mode) {
    const cfg = MODE_CFG[mode];
    const MC = { local: "var(--green)", direct: "var(--blue)", relay: "var(--accent)" }[mode];
    const isLocal = mode === "local";
    layer.style.setProperty("--mc", MC);

    const chip = atTop(layer, 960, 0, "chip");
    chip.innerHTML = `<span class="dot"></span>${mode.toUpperCase()}`;
    const ttl = atTop(layer, 960, 0, "title");
    ttl.textContent = cfg.title;
    const sub = atTop(layer, 960, 0, "lead");
    sub.style.fontSize = "30px";
    sub.textContent = cfg.sub;
    const state = atTop(layer, 960, STATE_Y, "mono state");
    state.style.fontSize = "28px";

    const dg = el("div", "diagram", layer);
    const s = svg("svg", { viewBox: "0 0 1920 1080", width: 1920, height: 1080 }, dg);

    const cl = cloud(s, 960, NODE_CY + 10, 0);

    // the network enclosure — one box around both devices (they share a
    // network), or one around each (they do not)
    const NET_ATTRS = { rx: 30, fill: "rgba(255,255,255,0.022)", stroke: "rgba(255,255,255,0.13)",
                        "stroke-width": 2, "stroke-dasharray": "10 12", opacity: 0 };
    const boxes = isLocal
      ? [{ x0: 184, x1: 1736 }]
      : [{ x0: LXC - 200, x1: LXC + 200 }, { x0: RXC - 200, x1: RXC + 200 }];
    const nets = boxes.map((b) => svg("rect", Object.assign({ x: b.x0, y: BOX_Y0,
      width: b.x1 - b.x0, height: BOX_Y1 - BOX_Y0 }, NET_ATTRS), s));
    const netLabels = isLocal
      ? [text(s, 960, BOX_Y0 - 26, "same network", "net-label", {})]
      : [text(s, LXC, BOX_Y0 - 26, "your network", "net-label", {}),
         text(s, RXC, BOX_Y0 - 26, "their network", "net-label", {})];
    const netSet = (a) => {
      nets.forEach((n) => n.setAttribute("opacity", a));
      netLabels.forEach((n) => { n.style.opacity = a; });
    };

    const devA = deviceCard(layer, LXC, DYC, "your device");
    const devB = deviceCard(layer, RXC, DYC, "their device");
    const node = hubCard(layer, 960, NODE_CY, "fsend.alzina.dev");
    node.classList.add("sm");
    if (isLocal) node.classList.add("ghost");
    const nodeTag = node.querySelector(".tag");

    // the evidence row: what this slide just proved
    const facts = [cfg.fact, { icon: LOCK, text: "end-to-end encrypted" }].map((it) => {
      const p = anchor(layer, 0, FACT_Y, "fact");
      p.innerHTML = `<span class="ic">${it.icon}</span>${it.text}`;
      p.querySelector(".ic").style.color = it.color || MC;
      return p;
    });
    facts[1].querySelector(".ic").style.color = "var(--green)";
    rowOf(facts, FACT_Y);

    // the wires, built once the metrics are known
    let wire, relay, halfA, halfB, xg, dA, dB, fWire, fRelay;
    let nodeBottom = NODE_CY + 92;
    afterFonts(() => {
      moveTo(chip, 88);
      moveTo(ttl, 88 + hOf(chip) + 30);
      moveTo(sub, 88 + hOf(chip) + 30 + hOf(ttl) + 24);
      nodeBottom = NODE_CY + hOf(node) / 2;

      const lp = [LX, DYC], rp = [RX, DYC];
      const nbl = [888, nodeBottom + 46], nbr = [1032, nodeBottom + 46];

      const dWire = `M ${lp[0]} ${lp[1]} L ${rp[0]} ${rp[1]}`;
      const dRelay = `M ${lp[0]} ${lp[1]} L ${nbl[0]} ${nbl[1]} L ${nbr[0]} ${nbr[1]} L ${rp[0]} ${rp[1]}`;

      wire = solidLink(s, dWire, MC, 6);
      wire.core.style.opacity = 0; wire.glow.style.opacity = 0;
      wire.core.dataset.len = sampler(dWire).length;
      fWire = makeFlow(s, sampler(dWire), { n: 3, r: 8, color: MC });

      relay = svg("path", { d: dRelay, fill: "none", stroke: MC, "stroke-width": 6, opacity: 0 }, s);
      relay.dataset.len = sampler(dRelay).length;
      const rGlow = svg("path", { d: dRelay, fill: "none", stroke: MC, "stroke-width": 20,
        opacity: 0 }, s);
      relay.dataset.glow = "1";
      relay.glowNode = rGlow;
      fRelay = makeFlow(s, sampler(dRelay), { n: 3, sealed: true, color: MC, size: 1.15 });

      // the introducer handshake (direct) / the blocked attempt (relay)
      const hy = DYC - 80;
      dA = dashLink(s, `M ${lp[0]} ${hy} L ${nbl[0]} ${nbl[1]}`, MC, 4);
      dB = dashLink(s, `M ${rp[0]} ${hy} L ${nbr[0]} ${nbr[1]}`, MC, 4);
      dA.setAttribute("opacity", 0); dB.setAttribute("opacity", 0);

      halfA = svg("path", { d: `M ${lp[0]} ${lp[1]} L 932 ${DYC}`, fill: "none",
        stroke: "var(--red)", "stroke-width": 5, opacity: 0 }, s);
      halfB = svg("path", { d: `M ${rp[0]} ${rp[1]} L 988 ${DYC}`, fill: "none",
        stroke: "var(--red)", "stroke-width": 5, opacity: 0 }, s);
      halfA.dataset.len = Math.hypot(932 - lp[0], 0);
      halfB.dataset.len = Math.hypot(988 - rp[0], 0);
      xg = svg("g", { opacity: 0 }, s);
      svg("circle", { cx: 960, cy: DYC, r: 34, fill: "#1a1110", stroke: "var(--red)",
        "stroke-width": 3 }, xg);
      svg("path", { d: `M 945 ${DYC - 15} L 975 ${DYC + 15} M 975 ${DYC - 15} L 945 ${DYC + 15}`,
        stroke: "var(--red)", "stroke-width": 5, "stroke-linecap": "round" }, xg);
    });

    const setTag = (txt, alpha) => {
      setText(nodeTag, txt);
      nodeTag.style.opacity = alpha;
    };

    return function (t) {
      reveal(chip, ramp(t, -0.66, -0.10));
      reveal(ttl, ramp(t, -0.62, -0.02), { dy: 20 });
      reveal(sub, ramp(t, -0.55, 0.05), { dy: 16, blur: 9 });
      const appear = ramp(t, -0.50, 0.08);
      netSet(0.9 * appear);
      reveal(devA, appear, { dy: 22 });
      reveal(devB, appear, { dy: 22 });
      cl.set(isLocal ? 0 : appear);

      // the middle slot is part of the stage, not an entrance: it is already
      // there on every slide, so the viewer reads the three routes as the same
      // picture with one element changing role
      reveal(node, appear, { dy: 16, scale: 0.98 });

      // the evidence row lands as soon as the route is live — it is what the
      // slide just proved, and it has to be on screen long enough to read
      const factAt = { local: api.on("local1", "connects") + 0.95,
                       direct: api.on("direct1", "connects") + 0.95,
                       relay: api.on("relay1", "relays") + 0.55 }[mode];
      facts.forEach((f, i) => reveal(f, ramp(t, factAt + i * 0.22, factAt + 0.55 + i * 0.22),
        { dy: 18 }));

      let lit = 0;   // how alive the two endpoints are

      if (isLocal) {
        /* one link, nothing in between ---------------------------------- */
        const on = api.on("local1", "connects");
        const linkP = A.easeOutCubic(ramp(t, on - 0.15, on + 0.6));
        lit = linkP;
        drawOn(wire, linkP);
        fWire.update(t, 0.42, ramp(t, on + 0.62, on + 1.05));
        setText(state, cfg.state);
        reveal(state, ramp(t, on + 0.7, on + 1.25));
        // the server card is already on stage — greyed out, unmistakably not
        // part of the path
        setTag("unused", 1);
      }

      if (mode === "direct") {
        /* pair, connect, let the server leave --------------------------- */
        const connects = api.on("direct1", "connects");
        const steps = api.on("direct2", "steps");

        // 1) the introducer: two dashed lines and a very short handshake.
        //    The lines retire the moment the direct link is up, so only one
        //    story is ever on screen at a time — but the server card stays, so
        //    the viewer can see it is still there, just out of the way.
        const shake = ramp(t, 0.9, 1.5) * (1 - ramp(t, connects - 0.10, connects + 0.55));
        setTag(t >= steps - 0.10 ? "steps aside" : "pairing", 1);
        dA.setAttribute("opacity", shake);
        dB.setAttribute("opacity", shake);
        if (shake > 0.01) {
          dA.setAttribute("stroke-dashoffset", (-t * 55).toFixed(1));
          dB.setAttribute("stroke-dashoffset", (t * 55).toFixed(1));
        }

        // 2) the direct link — the whole point of the slide
        const linkP = A.easeOutCubic(ramp(t, connects - 0.15, connects + 0.6));
        lit = linkP;
        drawOn(wire, linkP);
        fWire.update(t, 0.38, ramp(t, connects + 0.65, connects + 1.1));
        setText(state, cfg.state);
        reveal(state, ramp(t, connects + 0.7, connects + 1.2));
      }

      if (mode === "relay") {
        /* try direct, fail, relay --------------------------------------- */
        const blocked = api.on("relay1", "blocked");
        const relays = api.on("relay1", "relays");

        // 1) the direct attempt, and the wall it hits
        const att = A.easeOutCubic(ramp(t, 0.85, 1.8));
        const fade = 1 - ramp(t, relays - 0.45, relays + 0.15);
        if (att > 0) {
          const la = Number(halfA.dataset.len), lb = Number(halfB.dataset.len);
          halfA.setAttribute("opacity", att * fade);
          halfB.setAttribute("opacity", att * fade);
          halfA.setAttribute("stroke-dasharray", `${la * att} ${la}`);
          halfB.setAttribute("stroke-dasharray", `${lb * att} ${lb}`);
        }
        const stop = ramp(t, blocked - 0.30, blocked + 0.25) * fade;
        xg.setAttribute("opacity", (stop * (1 + 0.06 * Math.sin(t * 6))).toFixed(3));

        // 2) the relay takes it. The server was on stage all along (on
        //    standby while the direct attempt is tried), so the hand-off reads
        //    as the same card changing job.
        const rel = A.easeOutCubic(ramp(t, relays - 0.20, relays + 0.70));
        lit = rel;
        const sealedAt = api.on("relay2", "sealed");
        setTag(t >= sealedAt - 0.15 ? "ciphertext only"
                                   : t >= relays - 0.20 ? "relay" : "on standby", 1);
        cl.set(isLocal ? 0 : Math.max(appear, rel));
        relay.style.opacity = rel;
        relay.glowNode.style.opacity = 0.26 * rel;
        if (rel > 0) {
          const L = relay.dataset.len;
          relay.setAttribute("stroke-dasharray", `${L * rel} ${L}`);
          relay.glowNode.setAttribute("stroke-dasharray", `${L * rel} ${L}`);
        }
        fRelay.update(t, 0.26, ramp(t, relays + 0.75, relays + 1.3));

        // 3) the state line: blocked, then sealed
        const sealed = t >= relays + 0.30;
        setText(state, sealed ? cfg.state : "direct path blocked");
        state.style.color = sealed ? "var(--green)" : "var(--red)";
        reveal(state, Math.max(ramp(t, blocked - 0.05, blocked + 0.40), rel));
      }

      devA.style.setProperty("--lit", lit.toFixed(3));
      devB.style.setProperty("--lit", lit.toFixed(3));
    };
  }

  /* ====================================================================== *
   *  SCENE: cta — self-hosting
   * ====================================================================== */
  function cta(layer, api) {
    const head = atTop(layer, 960, 96, "h2");
    head.textContent = "Want full control? Run your own.";
    const sub = atTop(layer, 960, 0, "lead");
    sub.style.fontSize = "30px";
    sub.textContent = "the public server is free — no account, no setup";

    // beat 1+2: the public server and the only two things it does. Both roles
    // are on the card's own baseline, so the server reads as one object with a
    // short job description rather than two separate claims.
    const pub = anchor(layer, 960, 444, "card hub pub");
    pub.innerHTML = `<div class="glyph">${SERVER}</div><div class="name">fsend.alzina.dev</div>
      <div class="tag">free · no setup</div>`;

    const roles = [
      ["Pairs two devices", "then gets out of the way"],
      ["Relays the file", "only when direct is blocked"],
    ].map(([title, note], i) => {
      const r = anchor(layer, 960 + (i === 0 ? -272 : 272), 744, "card role");
      r.style.width = "500px";
      r.innerHTML = `<div class="rnum">${i + 1}</div><div><div class="rtitle">${title}</div>
        <div class="rnote">${note}</div></div>`;
      return r;
    });

    // beat 3: your own server, in the same slot — the frame is never empty
    const term = makeTerm(layer, 960, 900, "your server — zsh", [
      { text: "$ fsend server" },
      { text: "→ listening on :443", cls: "ok" },
    ]);
    moveTo(term.card, 512 - 108);
    const l2 = term.rows[1].node;

    const cards = [
      ["Docker image", "poliuscorp/fsend", CONTAINER],
      ["Self-hosting guide", "docs/self-hosting.md", DOC],
    ].map(([label, value, icon], i) => {
      const c = anchor(layer, 960 + (i === 0 ? -270 : 270), 730, "card inforow");
      c.style.width = "480px";
      c.innerHTML = `<div class="ic">${icon}</div><div><div class="ilabel">${label}</div>
        <div class="ivalue mono">${value}</div></div>`;
      return c;
    });
    const foot = anchor(layer, 960, 862, "mono");
    foot.style.fontSize = "26px";
    foot.style.color = "var(--dim)";
    foot.textContent = "github.com/polius/fsend";

    afterFonts(() => moveTo(sub, 96 + hOf(head) + 30));

    return function (t) {
      reveal(head, ramp(t, -0.62, -0.06));
      reveal(sub, ramp(t, 0.35, 1.05));

      const r0 = api.segStart("cta3");           // "run your own"
      const hand = ramp(t, r0 - 0.55, r0 + 0.35); // server -> terminal hand-off
      const out = 1 - hand;

      reveal(pub, ramp(t, -0.35, 0.45) * out, { dy: 22, scale: 0.97 });
      // the two roles land on the words that name them
      reveal(roles[0], ramp(t, api.on("cta2", "pairing") - 0.25, api.on("cta2", "pairing") + 0.45) * out,
        { dy: 18 });
      reveal(roles[1], ramp(t, api.on("cta2", "relaying") - 0.25, api.on("cta2", "relaying") + 0.45) * out,
        { dy: 18 });

      reveal(term.card, ramp(t, r0 + 0.05, r0 + 0.85), { dy: 22 });
      reveal(term.rows[0].node, ramp(t, r0 + 0.35, r0 + 0.75), { dy: 10, blur: 5 });
      reveal(l2, ramp(t, api.on("cta3", "control") + 0.05, api.on("cta3", "control") + 0.5),
        { dy: 10, blur: 5 });
      // the practical links follow the terminal immediately — the slide has to
      // be readable, not a race to the next one
      reveal(cards[0], ramp(t, r0 + 0.95, r0 + 1.55), { dy: 20 });
      reveal(cards[1], ramp(t, r0 + 1.30, r0 + 1.90), { dy: 20 });
      reveal(foot, ramp(t, r0 + 1.70, r0 + 2.30));
    };
  }

  /* ====================================================================== *
   *  SCENE: outro — the end card
   *
   *  Practical information first (how to get it), then the film's closing
   *  promise lands with the voice over the same card. No feature list: the
   *  last thing on screen is the name, the promise and where to find it.
   * ====================================================================== */
  function outro(layer, api) {
    const wm = anchor(layer, 960, 196);
    const w = A.wordmark(wm, 128);
    const under = anchor(layer, 960, 276);
    under.innerHTML = `<div class="underline"></div>`;
    alignUnderline(wm, under);

    const pill = anchor(layer, 960, 372, "install-pill");
    pill.innerHTML = `<span class="dot"></span>Easy to install`;

    const label = anchor(layer, 960, 466, "endlabel");
    label.textContent = "How to install";

    const rows = [
      ["Linux · macOS · FreeBSD", "curl -fsSL https://getfsend.alzina.dev | sh"],
      ["macOS (Homebrew)", "brew install polius/tap/fsend"],
      ["Windows (PowerShell)", "irm https://getfsend.alzina.dev/windows | iex"],
    ].map(([os, cmd], i) => {
      const r = anchor(layer, 960, 538 + i * 88, "install-row");
      r.style.width = "1320px";
      r.innerHTML = `<span class="os">${os}</span><span class="sep"></span><span class="cmd">${cmd}</span>`;
      return r;
    });

    const foot = anchor(layer, 960, 836, "endurl mono");
    foot.innerHTML = `getfsend.alzina.dev<span class="dot">·</span>github.com/polius/fsend`;

    return function (t) {
      const name = api.segStart("outro1");       // the wordmark and the name land together
      const linux = api.on("outro2", "linux");
      const windows = api.on("outro2", "windows");
      reveal(wm, ramp(t, -0.62, -0.06));
      if (w.cursor) w.cursor.style.opacity = Math.floor(t * 2) % 2 === 0 ? 1 : 0.05;
      under.querySelector(".underline").style.transform =
        `scaleX(${A.easeOutExpo(ramp(t, -0.45, 0.35)).toFixed(3)})`;
      // the card composes at a readable pace: badge, section label, then one
      // row at a time as the voice names the platforms
      reveal(pill, ramp(t, name + 0.40, name + 1.05), { dy: 16 });
      reveal(label, ramp(t, name + 1.00, name + 1.60), { dy: 12, blur: 7 });
      const runs = api.on("outro2", "runs");
      reveal(rows[0], ramp(t, runs + 0.05, runs + 0.60), { dy: 18 });
      reveal(rows[1], ramp(t, linux + 0.50, linux + 1.05), { dy: 18 });
      reveal(rows[2], ramp(t, windows - 0.15, windows + 0.40), { dy: 18 });
      reveal(foot, ramp(t, windows + 0.55, windows + 1.15));
    };
  }

  window.SCENES.brand = brand;
  window.SCENES.cmd = cmd;
  window.SCENES.modes = modes;
  window.SCENES.local = (l, a) => transferScene(l, a, "local");
  window.SCENES.direct = (l, a) => transferScene(l, a, "direct");
  window.SCENES.relay = (l, a) => transferScene(l, a, "relay");
  window.SCENES.cta = cta;
  window.SCENES.outro = outro;
})();
