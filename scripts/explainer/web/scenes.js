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

  // the three routes, drawn as a family so they can be read side by side.
  // each carries a `mflow` dot that travels the live path while the card is lit.
  function modeGlyph(kind) {
    const dev = (x) => `
      <rect x="${x}" y="78" width="58" height="42" rx="9" stroke="currentColor" stroke-width="3.2" opacity="0.85"/>
      <rect x="${x + 8}" y="86" width="42" height="26" rx="4" fill="currentColor" opacity="0.08"/>
      <rect x="${x + 12}" y="124" width="34" height="4" rx="2" fill="currentColor" opacity="0.3"/>`;
    const box_ = `<rect x="102" y="18" width="36" height="30" rx="9"
      stroke="currentColor" stroke-width="3.2"/>`;
    const head = `<svg width="364" height="212" viewBox="0 0 240 140" fill="none" aria-hidden="true">`;
    const devices = dev(10) + dev(172);
    if (kind === "local") {
      return head + devices +
        `<path d="M68 99h104" stroke="var(--green)" stroke-width="6" stroke-linecap="round"/>
         <circle class="mflow" cx="68" cy="99" r="7" fill="var(--green)" opacity="0"/>
         <circle cx="120" cy="99" r="9" fill="var(--green)"/></svg>`;
    }
    if (kind === "direct") {
      return head + `<g stroke="currentColor" stroke-width="3" fill="none" opacity="0.35">${box_}</g>` +
        devices +
        `<path d="M68 99L120 48M172 99L120 48" stroke="currentColor" stroke-width="2.4"
           stroke-dasharray="9 9" opacity="0.32"/>
         <path d="M68 99h104" stroke="var(--blue)" stroke-width="6" stroke-linecap="round"/>
         <circle class="mflow" cx="68" cy="99" r="7" fill="var(--blue)" opacity="0"/></svg>`;
    }
    return head + `<g fill="#17120f">${box_}</g>` + devices +
      `<path d="M68 99h104" stroke="currentColor" stroke-width="2.6" stroke-dasharray="2 11"
         stroke-linecap="round" opacity="0.35"/>
       <path d="M68 99L120 48M172 99L120 48" stroke="var(--accent)" stroke-width="6"
         stroke-linecap="round"/>
       <circle class="mflow" cx="68" cy="99" r="7" fill="var(--accent)" opacity="0"/></svg>`;
  }

  // where the `mflow` dot sits at u in [0,1) for each route
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
      n.textContent = l.text;
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
    tag.textContent = "Send any file, straight from one computer to another.";

    const pills = ["peer-to-peer", "end-to-end encrypted", "open source"].map((lab) => {
      const p = anchor(layer, 0, 706, "pill");
      p.textContent = lab;
      return p;
    });

    return function (t) {
      reveal(wm, ramp(t, 0.7, 1.55));
      if (w.cursor) w.cursor.style.opacity = Math.floor(t * 2) % 2 === 0 ? 1 : 0.05;
      const up = A.easeOutExpo(ramp(t, 0.92, 1.95));
      under.querySelector(".underline").style.transform = `scaleX(${up.toFixed(3)})`;
      reveal(tag, ramp(t, 2.45, 3.35));
      // pills: measure, then lay out centred
      const gap = 24;
      const widths = pills.map((p) => p.getBoundingClientRect().width);
      const total = widths.reduce((a, b) => a + b, 0) + gap * (pills.length - 1);
      let x = 960 - total / 2;
      pills.forEach((p, i) => {
        const outer = p.parentElement;
        outer.style.left = x + widths[i] / 2 + "px";
        outer.style.top = "706px";
        reveal(p, ramp(t, 3.5 + i * 0.16, 4.15 + i * 0.16),
          { ease: A.easeOutBack, dy: 16, scale: 0.9, blur: 6 });
        x += widths[i] + gap;
      });
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
      { text: "  share code:  abc-defg-jkm", cls: "acc" },
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
      status.textContent = done ? "✓ saved  drone-4k.mov   ·   112 MB/s"
                                : `receiving…  ${Math.round(p * 100)}%   ·   112 MB/s`;
      reveal(status, ramp(t, p0, p0 + 0.3));
    };
  }

  /* ====================================================================== *
   *  SCENE: modes — the three routes, lit as the narration names them
   * ====================================================================== */
  function modes(layer, api) {
    const head = atTop(layer, 960, 0, "h2");
    head.textContent = "How fsend works";

    const defs = [
      ["LOCAL", "local", "Direct", "same network", "works offline", "var(--green)"],
      ["DIRECT", "direct", "Direct", "across the internet", "server only introduces", "var(--blue)"],
      ["RELAY", "relay", "Relayed", "when direct is blocked", "ciphertext only", "var(--accent)"],
    ];
    const CW = 496, CGAP = 46;
    const x0 = 960 - (CW * 3 + CGAP * 2) / 2 + CW / 2;
    const kinds = defs.map((d) => d[1]);
    const flows = [];
    const cards = defs.map(([name, kind, desc, n1, n2, color], i) => {
      const card = atTop(layer, x0 + i * (CW + CGAP), 0, "card modecard");
      card.style.width = CW + "px";
      card.style.setProperty("--mc", color);
      card.innerHTML =
        `<div class="msweep"></div>
         <div class="mwrap">
           <div class="mglyph">${modeGlyph(kind)}</div>
           <div class="mlabel"><span class="mdot"></span>${name}</div>
           <div class="mdesc">${desc}</div>
           <div class="mnote">${n1}</div>
           <div class="mnote">${n2}</div>
           <div class="mcheck">✓</div>
         </div>`;
      flows.push(card.querySelector(".mflow"));
      return card;
    });

    afterFonts(() => {
      moveTo(head, 96);
      const top = 96 + hOf(head) + 92;
      cards.forEach((c) => moveTo(c, top));
    });

    return function (t) {
      reveal(head, ramp(t, -0.62, -0.06));
      cards.forEach((card, i) => {
        const appear = -0.72 - i * 0.12;
        const act = api.on("modes1", ["local", "direct", "relay"][i]);
        reveal(card, ramp(t, appear, appear + 0.55), { dy: 22, scale: 0.97 });
        const on = ramp(t, act - 0.06, act + 0.4);
        card.classList.toggle("active", on > 0.02);
        card.style.setProperty("--on", on.toFixed(3));
        // a packet runs the route while the card is lit
        const f = flows[i];
        if (f) {
          const u = ((t * 0.34) % 1 + 1) % 1;
          const [fx, fy] = MODE_FLOW[kinds[i]](u);
          f.setAttribute("cx", fx.toFixed(1));
          f.setAttribute("cy", fy.toFixed(1));
          f.setAttribute("opacity", (on * clamp(Math.min(u, 1 - u) / 0.09)).toFixed(3));
        }
      });
    };
  }

  /* ====================================================================== *
   *  the transfer diagram (local / direct / relay share all of this)
   * ====================================================================== */
  const LXC = 430, RXC = 1490, DYC = 725, HUBC = 392;
  const NET = { x0: 150, x1: 1770, y0: 600, y1: 850 };
  const BOXL = { x0: 168, x1: 692, y0: 600, y1: 850 };
  const BOXR = { x0: 1228, x1: 1752, y0: 600, y1: 850 };

  function transferScene(layer, api, mode) {
    const MC = { local: "var(--green)", direct: "var(--blue)", relay: "var(--accent)" }[mode];
    const SUB = { local: "same network · works offline",
                  direct: "over the internet",
                  relay: "over the internet · when a direct path is blocked" }[mode];

    const chip = atTop(layer, 960, 0, "chip");
    chip.innerHTML = `<span class="dot"></span>${mode.toUpperCase()}`;
    const sub = atTop(layer, 960, 0, "lead");
    sub.style.fontSize = "30px";
    sub.textContent = SUB;

    const dg = el("div", "diagram", layer);
    const s = svg("svg", { viewBox: "0 0 1920 1080", width: 1920, height: 1080 }, dg);

    const cl = cloud(s, 960, HUBC + 6, 0);
    // network enclosure(s)
    const NET_ATTRS = { rx: 28, fill: "rgba(255,255,255,0.028)", stroke: "rgba(255,255,255,0.13)",
                        "stroke-width": 2, "stroke-dasharray": "10 12", opacity: 0 };
    let netLabel1 = null, netLabel2 = null, nets = [];
    if (mode === "local") {
      nets = [svg("rect", Object.assign({ x: NET.x0, y: NET.y0, width: NET.x1 - NET.x0,
        height: NET.y1 - NET.y0 }, NET_ATTRS), s)];
      netLabel1 = text(s, 960, NET.y0 - 26, "same network", "net-label", { anchor: "middle" });
    } else {
      nets = [BOXL, BOXR].map((b) => svg("rect", Object.assign({ x: b.x0, y: b.y0,
        width: b.x1 - b.x0, height: b.y1 - b.y0 }, NET_ATTRS), s));
      netLabel1 = text(s, (BOXL.x0 + BOXL.x1) / 2, BOXL.y0 - 26, "your network", "net-label", {});
      netLabel2 = text(s, (BOXR.x0 + BOXR.x1) / 2, BOXR.y0 - 26, "their network", "net-label", {});
    }
    const netSet = (a) => nets.forEach((n) => n.setAttribute("opacity", a));

    // devices + hub
    const devA = deviceCard(layer, LXC, DYC, "your device");
    const devB = deviceCard(layer, RXC, DYC, "their device");
    const hub = hubCard(layer, 960, HUBC, mode === "local" ? "server" : "fsend.alzina.dev");
    const hubTag = hub.querySelector(".tag");
    const setHub = (state) => hub.style.setProperty("--hub", state);

    const cap = atTop(layer, 960, 0, "mono");
    cap.style.fontSize = "28px";
    cap.style.color = "var(--dim)";
    const cap2 = atTop(layer, 960, 0);
    cap2.innerHTML = `<div class="lockrow"><span class="lock">${LOCK}</span>End-to-end encrypted</div>`;

    // everything below is built once the real font metrics are known, so the
    // links always meet the cards exactly
    let direct, dA, dB, relayPath, relayGlow, halfA, halfB, xg;
    let fDirect, fA, fB, fRelay;

    afterFonts(() => {
      moveTo(chip, 100);
      moveTo(sub, 100 + hOf(chip) + 30);
      // the hub card is centre-anchored, so give it its centre
      const hubH = hOf(hub);
      const hubTop = 100 + hOf(chip) + 30 + hOf(sub) + 58;
      moveTo(hub, hubTop + hubH / 2);
      const hubBottom = hubTop + hubH;
      const svY = hubBottom + 26;
      const lp = [LXC + 160, DYC], rp = [RXC - 160, DYC];
      const svl = [960 - 92, svY], svr = [960 + 92, svY];

      const dDirect = `M ${lp[0]} ${lp[1]} L ${rp[0]} ${rp[1]}`;
      const dRelay = `M ${lp[0]} ${lp[1]} L ${svl[0]} ${svl[1]} L ${svr[0]} ${svr[1]} L ${rp[0]} ${rp[1]}`;
      const smpDirect = sampler(dDirect), smpRelay = sampler(dRelay);
      const smpA = sampler(`M ${lp[0]} ${lp[1]} L ${svl[0]} ${svl[1]}`);
      const smpB = sampler(`M ${rp[0]} ${rp[1]} L ${svr[0]} ${svr[1]}`);

      direct = solidLink(s, dDirect, MC, 6);
      direct.glow.style.opacity = 0; direct.core.style.opacity = 0;
      dA = dashLink(s, `M ${lp[0]} ${lp[1]} L ${svl[0]} ${svl[1]}`, MC, 4);
      dB = dashLink(s, `M ${rp[0]} ${rp[1]} L ${svr[0]} ${svr[1]}`, MC, 4);
      dA.setAttribute("opacity", 0); dB.setAttribute("opacity", 0);
      relayPath = svg("path", { d: dRelay, fill: "none", stroke: MC, "stroke-width": 6, opacity: 0 }, s);
      relayGlow = svg("path", { d: dRelay, fill: "none", stroke: MC, "stroke-width": 20,
        opacity: 0, class: "spark" }, s);

      // the "direct attempt" (relay scene) — two halves that meet in the middle
      halfA = svg("path", { d: `M ${lp[0]} ${lp[1]} L 930 ${DYC}`, fill: "none",
        stroke: "var(--red)", "stroke-width": 5, opacity: 0 }, s);
      halfB = svg("path", { d: `M ${rp[0]} ${rp[1]} L 990 ${DYC}`, fill: "none",
        stroke: "var(--red)", "stroke-width": 5, opacity: 0 }, s);
      xg = svg("g", { opacity: 0 }, s);
      svg("circle", { cx: 960, cy: DYC, r: 34, fill: "#1a1110", stroke: "var(--red)",
        "stroke-width": 3 }, xg);
      svg("path", { d: `M 945 ${DYC - 15} L 975 ${DYC + 15} M 975 ${DYC - 15} L 945 ${DYC + 15}`,
        stroke: "var(--red)", "stroke-width": 5, "stroke-linecap": "round" }, xg);

      fDirect = makeFlow(s, smpDirect, { n: 3, r: 8, color: MC });
      fA = makeFlow(s, smpA, { n: 2, r: 7, color: MC });
      fB = makeFlow(s, smpB, { n: 2, r: 7, color: MC });
      fRelay = makeFlow(s, smpRelay, { n: 3, sealed: true, color: MC, size: 1.15 });

      // lengths for the "draw on" dash reveal
      direct.core.dataset.len = smpDirect.length;
      relayPath.dataset.len = smpRelay.length;
      halfA.dataset.len = Math.hypot(930 - lp[0], DYC - lp[1]);
      halfB.dataset.len = Math.hypot(990 - rp[0], DYC - rp[1]);

      moveTo(cap, DYC + 160);
      moveTo(cap2, DYC + 218);
    });

    return function (t) {
      reveal(chip, ramp(t, -0.62, -0.06));
      reveal(sub, ramp(t, -0.52, 0.05));
      const appear = ramp(t, -0.58, 0.04);
      netSet(0.9 * appear);
      if (netLabel1) netLabel1.style.opacity = appear;
      if (netLabel2) netLabel2.style.opacity = appear;
      reveal(devA, appear, { dy: 24 });
      reveal(devB, appear, { dy: 24 });
      reveal(hub, appear, { dy: -18, scale: 0.96 });
      // the local path never leaves the LAN, so there is no internet cloud
      cl.set(mode === "local" ? 0 : appear);
      setHub(mode === "local" ? 0.25 : 1);
      let lit = 0;   // how alive the two endpoints are

      if (mode === "local") {
        const linkP = A.easeOutCubic(ramp(t, api.on("local1", "connects") - 0.15, api.on("local1", "connects") + 0.6));
        lit = linkP;
        direct.core.style.opacity = linkP;
        direct.glow.style.opacity = 0.30 * linkP;
        if (linkP > 0) {
          const L = direct.core.dataset.len;
          direct.core.setAttribute("stroke-dasharray", `${L * linkP} ${L}`);
          direct.glow.setAttribute("stroke-dasharray", `${L * linkP} ${L}`);
        }
        fDirect.update(t, 0.42, ramp(t, api.on("local1", "connects") + 0.65, api.on("local1", "connects") + 1.1));
        cap.textContent = "direct over the LAN";
        reveal(cap, ramp(t, api.on("local1", "connects") + 0.7, api.on("local1", "connects") + 1.3));
        reveal(cap2, ramp(t, 3.3, 3.9));
        // the transfer never goes through the server (the sender does register
        // a session, so "not contacted" would over-claim — it is simply unused)
        const off = ramp(t, api.on("local1", "noserver") - 0.35, api.on("local1", "noserver") + 0.3);
        hubTag.textContent = "not used";
        hubTag.style.opacity = off;
        setHub(0.22 * (1 - off));
      }

      if (mode === "direct") {
        // 1) a quick pairing handshake, 2) the direct link, 3) server steps aside
        const pairA = ramp(t, 0.95, 1.5) * (1 - ramp(t, 1.75, 2.1));
        dA.setAttribute("opacity", pairA);
        dB.setAttribute("opacity", pairA);
        if (pairA > 0.01) { dA.setAttribute("stroke-dashoffset", (-t * 60).toFixed(1)); dB.setAttribute("stroke-dashoffset", (t * 60).toFixed(1)); }
        const linkP = A.easeOutCubic(ramp(t, api.on("direct1", "connects") - 0.15, api.on("direct1", "connects") + 0.65));
        lit = linkP;
        direct.core.style.opacity = linkP; direct.glow.style.opacity = 0.30 * linkP;
        if (linkP > 0) { const L = direct.core.dataset.len;
          direct.core.setAttribute("stroke-dasharray", `${L * linkP} ${L}`);
          direct.glow.setAttribute("stroke-dasharray", `${L * linkP} ${L}`); }
        fDirect.update(t, 0.36, ramp(t, api.on("direct1", "connects") + 0.7, api.on("direct1", "connects") + 1.15));
        cap.textContent = "direct, peer-to-peer";
        reveal(cap, ramp(t, 2.55, 3.15));
        reveal(cap2, ramp(t, 3.3, 3.9));
        // the server's only job: introductions — shown, then withdrawn
        const intro = ramp(t, api.on("direct1", "introduces") - 0.3, api.on("direct1", "introduces") + 0.4) *
                      (1 - ramp(t, api.on("direct1", "steps") - 0.3, api.on("direct1", "steps") + 0.5));
        hubTag.textContent = "pairing only";
        hubTag.style.opacity = intro;
        setHub(lerp(1, 0.3, ramp(t, api.on("direct1", "steps") - 0.3, api.on("direct1", "steps") + 0.5)));
        const away = ramp(t, api.on("direct1", "introduces"), api.on("direct1", "introduces") + 0.4) *
                     (1 - ramp(t, api.on("direct1", "steps") - 0.4, api.on("direct1", "steps") + 0.3));
        dA.setAttribute("opacity", away); dB.setAttribute("opacity", away);
        if (away > 0.01) { dA.setAttribute("stroke-dashoffset", (-t * 60).toFixed(1)); dB.setAttribute("stroke-dashoffset", (t * 60).toFixed(1)); }
      }

      if (mode === "relay") {
        // the direct attempt fails …
        const att = A.easeOutCubic(ramp(t, 0.95, 1.85));
        if (att > 0) {
          const la = Number(halfA.dataset.len), lb = Number(halfB.dataset.len);
          halfA.setAttribute("opacity", att); halfB.setAttribute("opacity", att);
          halfA.setAttribute("stroke-dasharray", `${la * att} ${la}`);
          halfB.setAttribute("stroke-dasharray", `${lb * att} ${lb}`);
        }
        const blocked = ramp(t, api.on("relay1", "blocked") - 0.2, api.on("relay1", "blocked") + 0.35);
        const fadeAttempt = 1 - ramp(t, api.on("relay1", "relay") - 0.5, api.on("relay1", "relay") + 0.2);
        xg.setAttribute("opacity", blocked * fadeAttempt * (1 + 0.06 * Math.sin(t * 6)));
        halfA.setAttribute("opacity", att * fadeAttempt);
        halfB.setAttribute("opacity", att * fadeAttempt);
        // … so it relays, end to end encrypted
        const rp2 = A.easeOutCubic(ramp(t, api.on("relay1", "relay") - 0.2, api.on("relay1", "relay") + 0.7));
        lit = rp2;
        relayPath.setAttribute("opacity", rp2); relayGlow.setAttribute("opacity", 0.26 * rp2);
        if (rp2 > 0) { const L = relayPath.dataset.len;
          relayPath.setAttribute("stroke-dasharray", `${L * rp2} ${L}`);
          relayGlow.setAttribute("stroke-dasharray", `${L * rp2} ${L}`); }
        fRelay.update(t, 0.24, ramp(t, api.on("relay1", "relay") + 0.75, api.on("relay1", "relay") + 1.3));
        const sealed = ramp(t, api.on("relay1", "sealed") - 0.3, api.on("relay1", "sealed") + 0.4);
        const capRelay = ramp(t, api.on("relay1", "relay") + 0.8, api.on("relay1", "relay") + 1.4);
        if (capRelay > 0) {
          cap.textContent = sealed > 0.5 ? "relayed — ciphertext only" : "relaying through the server";
          cap.style.color = sealed > 0.5 ? "var(--green)" : "var(--dim)";
        } else {
          cap.textContent = "direct path blocked";
          cap.style.color = "var(--red)";
        }
        reveal(cap, Math.max(ramp(t, api.on("relay1", "blocked"), api.on("relay1", "blocked") + 0.5), capRelay));
        hubTag.textContent = "relay";
        hubTag.style.opacity = rp2;
        reveal(cap2, ramp(t, api.on("relay1", "sealed") + 0.15, api.on("relay1", "sealed") + 0.7));
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
    sub.textContent = "the public server is free — and needs no setup";

    // beat 1: the public server itself, sitting in the same slot the terminal
    // later takes over — so the frame is never empty and the hand-off reads.
    const pub = anchor(layer, 960, 520, "card hub pub");
    pub.innerHTML = `<div class="glyph">${SERVER}</div><div class="name">fsend.alzina.dev</div>
      <div class="tag">free · no setup</div>`;

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
      reveal(sub, ramp(t, 1.28, 1.98));
      const r0 = api.segStart("cta2");
      // the two beats cross-fade in the same slot, so the stage is never empty
      reveal(pub, ramp(t, 1.05, 1.8) * (1 - ramp(t, r0 - 0.55, r0 + 0.35)),
        { dy: 20, scale: 0.95 });
      reveal(term.card, ramp(t, r0 + 0.05, r0 + 0.85), { dy: 22 });
      reveal(term.rows[0].node, ramp(t, r0 + 0.35, r0 + 0.75), { dy: 10, blur: 5 });
      reveal(l2, ramp(t, api.on("cta2", "control") + 0.05, api.on("cta2", "control") + 0.5), { dy: 10, blur: 5 });
      reveal(cards[0], ramp(t, 5.05, 5.75), { dy: 20 });
      reveal(cards[1], ramp(t, 5.7, 6.4), { dy: 20 });
      reveal(foot, ramp(t, 6.1, 6.8));
    };
  }

  /* ====================================================================== *
   *  SCENE: outro — install + sign-off
   * ====================================================================== */
  function outro(layer, api) {
    const wm = anchor(layer, 960, 226);
    const w = A.wordmark(wm, 140);
    const under = anchor(layer, 960, 314);
    under.innerHTML = `<div class="underline"></div>`;
    alignUnderline(wm, under);
    const head = anchor(layer, 960, 392);
    head.className += " lead";
    head.style.fontSize = "28px";
    head.style.letterSpacing = "0.14em";
    head.style.color = "var(--dim)";
    head.textContent = "HOW TO INSTALL";

    const rows = [
      ["Linux · macOS · FreeBSD", "curl -fsSL https://getfsend.alzina.dev | sh"],
      ["macOS (Homebrew)", "brew install polius/tap/fsend"],
      ["Windows (PowerShell)", "irm https://getfsend.alzina.dev/windows | iex"],
    ].map(([os, cmd], i) => {
      const r = anchor(layer, 960, 476 + i * 88, "install-row");
      r.style.width = "1320px";
      r.innerHTML = `<span class="os">${os}</span><span class="sep"></span><span class="cmd">${cmd}</span>`;
      return r;
    });

    const pills = ["Simple", "Private", "Free"].map((w2) => {
      const p = anchor(layer, 0, 822, "pill");
      p.style.fontSize = "30px";
      p.textContent = w2;
      return p;
    });
    const foot = anchor(layer, 960, 914, "mono");
    foot.style.fontSize = "26px";
    foot.style.color = "var(--dim)";
    foot.textContent = "github.com/polius/fsend";

    return function (t) {
      reveal(wm, ramp(t, -0.62, -0.06));
      if (w.cursor) w.cursor.style.opacity = Math.floor(t * 2) % 2 === 0 ? 1 : 0.05;
      under.querySelector(".underline").style.transform =
        `scaleX(${A.easeOutExpo(ramp(t, -0.45, 0.35)).toFixed(3)})`;
      reveal(head, ramp(t, -0.32, 0.28), { dy: 14, blur: 8 });
      rows.forEach((r, i) => reveal(r, ramp(t, -0.24 + i * 0.12, 0.3 + i * 0.12), { dy: 20 }));
      reveal(foot, ramp(t, 0.1, 0.7));
      // the sign-off lands on the spoken words
      const gap = 24;
      const widths = pills.map((p) => p.getBoundingClientRect().width);
      const total = widths.reduce((a, b) => a + b, 0) + gap * (pills.length - 1);
      let x = 960 - total / 2;
      pills.forEach((p, i) => {
        const at = api.on("outro2", ["simple", "private", "free"][i]);
        const outer = p.parentElement;
        outer.style.left = x + widths[i] / 2 + "px";
        outer.style.top = "822px";
        reveal(p, ramp(t, at, at + 0.32), { ease: A.easeOutBack, dy: 14, scale: 0.88, blur: 5 });
        x += widths[i] + gap;
      });
      // settle: after the sign-off, dim the install block and let the end hold
      const settle = ramp(t, api.on("outro2", "free") + 0.8, api.on("outro2", "free") + 2.0);
      [head, under, foot, ...rows].forEach((n) => { n.style.opacity = (1 - 0.4 * settle).toFixed(3); });
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
