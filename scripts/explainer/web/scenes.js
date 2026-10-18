/* fsend explainer — the scenes.
 *
 * Every scene is a function (layer, api) -> render(t) where t is seconds since
 * the scene started. Nothing is animated by CSS: render(t) computes the state of
 * every moving part from the timeline onsets, so a frame is a pure function of t.
 */
(function () {
  const A = window.Toolkit;
  const { el, svg, text, box, reveal, ramp, lerp, clamp } = A;

  /* ------------------------------------------------------------------ *
   *  shared bits
   * ------------------------------------------------------------------ */

  // an absolutely-placed, centre-anchored element whose *own* transform is free
  function anchor(layer, x, y, cls) {
    const outer = box(layer, x, y);
    const inner = el("div", "rv" + (cls ? " " + cls : ""), outer);
    return inner;
  }
  function layerPos(x, y) { return `left:${x}px;top:${y}px`; }
  // left-anchored variant (for badges that sit above a card's left edge)
  function anchorLeft(layer, x, y, cls) {
    const outer = box(layer, x, y);
    outer.style.transform = "translate(0, -50%)";
    const inner = el("div", "rv" + (cls ? " " + cls : ""), outer);
    return inner;
  }

  const LAPTOP = `<svg width="118" height="88" viewBox="0 0 118 88" fill="none" aria-hidden="true">
    <rect x="15" y="6" width="88" height="60" rx="9" stroke="currentColor" stroke-width="3.4" opacity="0.9"/>
    <rect x="23" y="14" width="72" height="44" rx="5" fill="var(--accent)" opacity="0.10"/>
    <text x="59" y="38" text-anchor="middle" dominant-baseline="central"
      font-family="JBMono" font-size="21" font-weight="700" fill="var(--accent-l)">&gt;_</text>
    <rect x="4" y="71" width="110" height="9" rx="4.5" fill="currentColor" opacity="0.55"/>
  </svg>`;

  const SERVER = `<svg width="92" height="88" viewBox="0 0 92 88" fill="none" aria-hidden="true">
    <rect x="8" y="4" width="76" height="80" rx="10" stroke="currentColor" stroke-width="3.2" opacity="0.9"/>
    ${[0, 1, 2].map((i) => {
      const y = 18 + i * 22;
      return `<rect x="18" y="${y}" width="56" height="15" rx="4" fill="currentColor" opacity="0.12"/>
              <circle cx="27" cy="${y + 7.5}" r="3.4" fill="var(--accent)"/>`;
    }).join("")}
  </svg>`;

  function deviceCard(layer, x, y, name) {
    const card = anchor(layer, x, y, "card dev");
    card.style.cssText += layerPos(0, 0);
    card.innerHTML = `<div class="glyph">${LAPTOP}</div><div class="name">${name}</div>`;
    return card;
  }
  function hubCard(layer, x, y, name, tag) {
    const card = anchor(layer, x, y, "card hub");
    card.innerHTML = `<div class="glyph">${SERVER}</div>
      <div class="name">${name}</div>${tag ? `<div class="tag">${tag}</div>` : ""}`;
    return card;
  }

  /* SVG link / markers ---------------------------------------------------- */
  function dashLink(parent, d, color, width) {
    const p = svg("path", { d, fill: "none", stroke: color, "stroke-width": width,
      "stroke-dasharray": "14 16", "stroke-linecap": "round" }, parent);
    return p;
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

  /* flowing packets ------------------------------------------------------- */
  function makeFlow(parent, d, opts) {
    const { n = 3, r = 9, color = "#fff", sealed = false, size = 1 } = opts;
    const path = svg("path", { d, fill: "none", stroke: "none" }, parent);
    const L = path.getTotalLength();
    const items = [];
    for (let i = 0; i < n; i++) {
      let node;
      if (!sealed) {
        const g = svg("g", {}, parent);
        svg("circle", { r: r * 2.8, fill: color, opacity: 0.12 }, g);
        svg("circle", { r: r * 1.7, fill: color, opacity: 0.26 }, g);
        svg("circle", { r, fill: color }, g);
        node = g;
      } else {
        const g = svg("g", {}, parent);
        const w = 34 * size, h = 24 * size;
        svg("circle", { r: r * 2.4, fill: color, opacity: 0.14 }, g);
        svg("rect", { x: -w / 2, y: -h / 2, width: w, height: h, rx: 4 * size,
          fill: "#120e0c", stroke: color, "stroke-width": 2.2 }, g);
        svg("path", { d: `M ${-w / 2} ${-h / 2 + 2} L 0 ${2 * size} L ${w / 2} ${-h / 2 + 2}`,
          fill: "none", stroke: color, "stroke-width": 2.2 }, g);
        svg("circle", { cx: 0, cy: 2 * size, r: 3.1 * size, fill: color }, g);
        node = g;
      }
      items.push(node);
      node.style.opacity = 0;   // nothing shows until update() runs
    }
    return {
      update(t, speed, alpha) {
        if (alpha <= 0.001) { items.forEach((n) => (n.style.opacity = 0)); return; }
        items.forEach((node, i) => {
          const u = ((t * speed + i / items.length) % 1 + 1) % 1;
          const pt = path.getPointAtLength(u * L);
          node.setAttribute("transform", `translate(${pt.x.toFixed(2)},${pt.y.toFixed(2)})`);
          // fade in/out at the ends for a clean entry/exit
          const edge = Math.min(1, Math.min(u, 1 - u) / 0.12);
          node.style.opacity = (alpha * clamp(edge)).toFixed(3);
        });
      },
      hide() { items.forEach((n) => (n.style.opacity = 0)); },
    };
  }

  /* a soft cloud behind the hub (gradient, so no blur filter is needed) ---- */
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
    const wm = anchor(layer, 960, 452);
    const w = A.wordmark(wm, 150);
    const under = anchor(layer, 960, 545);
    under.innerHTML = `<div class="underline"></div>`;
    under.style.cssText += "transform-origin:50% 50%";
    const tag = anchor(layer, 960, 610);
    tag.className += " lead";
    tag.style.textAlign = "center";
    tag.textContent = "Send any file, straight from one computer to another.";

    const labels = ["peer-to-peer", "end-to-end encrypted", "open source"];
    const pills = labels.map((lab, i) => {
      const p = anchor(layer, 0, 706, "pill");
      p.textContent = lab;
      return p;
    });

    return function (t) {
      reveal(wm, ramp(t, 0.75, 1.6));
      const cur = wm.querySelector(".cur");
      if (cur) cur.style.opacity = Math.floor(t * 2) % 2 === 0 ? 1 : 0.05;
      const up = A.easeOutExpo(ramp(t, 0.95, 2.0));
      under.querySelector(".underline").style.transform = `scaleX(${up.toFixed(3)})`;
      reveal(tag, ramp(t, 2.5, 3.4));
      // pills: measure, then lay out centred
      const gap = 24;
      const widths = pills.map((p) => p.getBoundingClientRect().width);
      const total = widths.reduce((a, b) => a + b, 0) + gap * (pills.length - 1);
      let x = 960 - total / 2;
      pills.forEach((p, i) => {
        const outer = p.parentElement;
        outer.style.left = x + widths[i] / 2 + "px";
        outer.style.top = "712px";
        reveal(p, ramp(t, 3.5 + i * 0.16, 4.15 + i * 0.16), { ease: A.easeOutBack, dy: 16, scale: 0.9, blur: 6 });
        x += widths[i] + gap;
      });
    };
  }

  /* ====================================================================== *
   *  SCENE: cmd — the two terminals
   * ====================================================================== */
  function cmd(layer, api) {
    const head = anchor(layer, 960, 132, "h2");
    head.textContent = "One command to send, one to receive.";
    head.style.fontSize = "54px";

    const CW = 1080;
    function term(x, y, name, lines) {
      const c = anchor(layer, x, y, "term");
      c.style.width = CW + "px";
      c.innerHTML = `<div class="bar"><i class="r"></i><i class="y"></i><i class="g"></i>
        <span class="name">${name}</span></div><div class="body"></div>`;
      const body = c.querySelector(".body");
      const nodes = lines.map((l) => {
        const n = el("div", "ln" + (l.cls ? " " + l.cls : ""), body);
        n.textContent = l.text;
        n.style.visibility = "hidden";
        return n;
      });
      return { card: c, nodes };
    }

    const sBadge = anchorLeft(layer, 420, 268, "badge");
    sBadge.textContent = "Sender";
    const sender = term(960, 452, "you — zsh", [
      { text: "$ fsend drone-4k.mov" },
      { text: "  drone-4k.mov   ·   2.3 GB", cls: "dim" },
      { text: "  share code:  abc-defg-jkm", cls: "acc" },
    ]);

    const rBadge = anchorLeft(layer, 420, 620, "badge ok");
    rBadge.textContent = "Receiver";
    const recv = term(960, 800, "your friend — zsh", [
      { text: "$ fsend abc-defg-jkm" },
      { text: "  from your friend   ·   accept? y", cls: "dim" },
    ]);

    const bar = anchor(layer, 960, 948, "");
    bar.style.width = CW - 68 + "px";
    bar.innerHTML = `<div class="bar-track"><div class="bar-fill"></div></div>`;
    const fill = bar.querySelector(".bar-fill");
    const status = anchor(layer, 960, 992, "mono");
    status.style.fontSize = "28px";

    return function (t) {
      reveal(head, ramp(t, -0.6, -0.05));
      reveal(sBadge, ramp(t, -0.55, -0.05));
      reveal(sender.card, ramp(t, -0.5, 0.0));
      reveal(sender.nodes[0], ramp(t, -0.35, 0.12), { dy: 12, blur: 6 });
      reveal(sender.nodes[1], ramp(t, 1.2, 1.7), { dy: 12, blur: 6 });
      reveal(sender.nodes[2], ramp(t, api.on("cmd1", "code") - 0.2, api.on("cmd1", "code") + 0.35), { dy: 12, blur: 6 });

      const r0 = api.segStart("cmd2");
      reveal(rBadge, ramp(t, r0, r0 + 0.55));
      reveal(recv.card, ramp(t, r0 + 0.08, r0 + 0.85));
      reveal(recv.nodes[0], ramp(t, r0 + 0.35, r0 + 0.8), { dy: 12, blur: 6 });
      reveal(recv.nodes[1], ramp(t, api.on("cmd2", "runs") - 0.25, api.on("cmd2", "runs") + 0.2), { dy: 12, blur: 6 });

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
   *  SCENE: modes — the three paths, revealed as the narration names them
   * ====================================================================== */
  function modes(layer, api) {
    const head = anchor(layer, 960, 172, "h2");
    head.textContent = "How fsend works";
    const defs = [
      ["LOCAL", "same network → connect directly", "server not needed · works offline", "var(--green)"],
      ["DIRECT", "across the internet → connect directly", "server only introduces them", "var(--blue)"],
      ["RELAY", "across the internet → relayed", "relayed data is always encrypted", "var(--accent)"],
    ];
    const cards = defs.map(([name, desc, note, color], i) => {
      const card = anchor(layer, 960, 360 + i * 150, "card modecard");
      card.style.cssText += `--mc:${color}`;
      card.innerHTML =
        `<div class="mlabel"><span class="mdot"></span>${name}</div>
         <div class="mdiv"></div>
         <div class="mtext"><div class="mdesc">${desc}</div><div class="mnote">${note}</div></div>
         <div class="mcheck">✓</div>`;
      return card;
    });
    const foot = anchor(layer, 960, 880, "");
    foot.innerHTML = `<div class="lockrow"><span class="lock">${LOCK}</span>End-to-end encrypted on every path</div>`;

    return function (t) {
      reveal(head, ramp(t, -0.62, -0.06));
      cards.forEach((card, i) => {
        const appear = -0.72 - i * 0.12;
        const act = api.on("modes1", ["local", "direct", "relay"][i]);
        // ghost in
        reveal(card, ramp(t, appear, appear + 0.55), { dy: 20, scale: 0.97 });
        const on = ramp(t, act - 0.06, act + 0.4);
        card.classList.toggle("active", on > 0.02);
        card.style.setProperty("--on", on.toFixed(3));
      });
      reveal(foot, ramp(t, api.on("modes1", "relay") + 0.4, api.on("modes1", "relay") + 1.1), { dy: 16, blur: 8 });
    };
  }

  /* ====================================================================== *
   *  the transfer diagram (local / direct / relay share all of this)
   * ====================================================================== */
  const LXC = 430, RXC = 1490, DYC = 705, HUBC = 355;
  const NET = { x0: 150, x1: 1770, y0: 580, y1: 830 };
  const BOXL = { x0: 168, x1: 692, y0: 580, y1: 830 };
  const BOXR = { x0: 1228, x1: 1752, y0: 580, y1: 830 };

  function transferScene(layer, api, mode) {
    const MC = { local: "var(--green)", direct: "var(--blue)", relay: "var(--accent)" }[mode];
    const SUB = { local: "same network · works offline",
                  direct: "over the internet",
                  relay: "over the internet · when a direct path is blocked" }[mode];

    const chip = anchor(layer, 960, 150, "chip");
    chip.innerHTML = `<span class="dot"></span>${mode.toUpperCase()}`;
    const sub = anchor(layer, 960, 214);
    sub.className += " lead";
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
    const hub = hubCard(layer, 960, HUBC, mode === "local" ? "server" : "fsend.alzina.dev", "");

    // links. The hub geometry is measured after layout so the links always meet
    // the card's bottom edge, whatever its content makes it.
    const hr = hub.getBoundingClientRect();
    const svY = hr.bottom + 26;
    const lp = [LXC + 160, DYC], rp = [RXC - 160, DYC];
    const svl = [960 - 92, svY], svr = [960 + 92, svY];
    const direct = solidLink(s, `M ${lp[0]} ${lp[1]} L ${rp[0]} ${rp[1]}`, MC, 6);
    direct.glow.style.opacity = 0; direct.core.style.opacity = 0;
    const dA = dashLink(s, `M ${lp[0]} ${lp[1]} L ${svl[0]} ${svl[1]}`, MC, 4); dA.setAttribute("opacity", 0);
    const dB = dashLink(s, `M ${rp[0]} ${rp[1]} L ${svr[0]} ${svr[1]}`, MC, 4); dB.setAttribute("opacity", 0);
    const relD = `M ${lp[0]} ${lp[1]} L ${svl[0]} ${svl[1]} L ${svr[0]} ${svr[1]} L ${rp[0]} ${rp[1]}`;
    const relayPath = svg("path", { d: relD, fill: "none", stroke: MC, "stroke-width": 6, opacity: 0 }, s);
    const relayGlow = svg("path", { d: relD, fill: "none", stroke: MC, "stroke-width": 20, opacity: 0, class: "spark" }, s);

    // the "direct attempt" (relay scene) — two halves that meet in the middle
    const halfA = svg("path", { d: `M ${lp[0]} ${lp[1]} L 930 ${DYC}`, fill: "none", stroke: "var(--red)", "stroke-width": 5, opacity: 0 }, s);
    const halfB = svg("path", { d: `M ${rp[0]} ${rp[1]} L 990 ${DYC}`, fill: "none", stroke: "var(--red)", "stroke-width": 5, opacity: 0 }, s);
    const xg = svg("g", { opacity: 0 }, s);
    svg("circle", { cx: 960, cy: DYC, r: 34, fill: "#1a1110", stroke: "var(--red)", "stroke-width": 3 }, xg);
    svg("path", { d: `M 945 ${DYC - 15} L 975 ${DYC + 15} M 975 ${DYC - 15} L 945 ${DYC + 15}`, stroke: "var(--red)", "stroke-width": 5, "stroke-linecap": "round" }, xg);

    // packet streams
    const fDirect = makeFlow(s, `M ${lp[0]} ${lp[1]} L ${rp[0]} ${rp[1]}`, { n: 3, r: 8, color: MC });
    const fA = makeFlow(s, `M ${lp[0]} ${lp[1]} L ${svl[0]} ${svl[1]}`, { n: 2, r: 7, color: MC });
    const fB = makeFlow(s, `M ${rp[0]} ${rp[1]} L ${svr[0]} ${svr[1]}`, { n: 2, r: 7, color: MC });
    const fRelay = makeFlow(s, relD, { n: 3, sealed: true, color: MC, size: 1.15 });

    // captions
    const cap = anchor(layer, 960, DYC + 172, "mono");
    cap.style.fontSize = "28px";
    cap.style.color = "var(--dim)";
    const cap2 = anchor(layer, 960, DYC + 236, "");
    cap2.innerHTML = `<div class="lockrow"><span class="lock">${LOCK}</span>End-to-end encrypted</div>`;
    const tagOnHub = (txt) => {
      const t = hub.querySelector(".tag");
      if (t) t.textContent = txt;
    };
    const hubTag = el("div", "tag", null);
    hubTag.textContent = "";
    hub.appendChild(hubTag);

    function setHub(state) {
      hub.style.setProperty("--hub", state);   // 1 = lit, 0 = dormant
    }

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

      if (mode === "local") {
        // the link draws on, then packets flow
        const linkP = A.easeOutCubic(ramp(t, api.on("local1", "connects") - 0.15, api.on("local1", "connects") + 0.6));
        direct.core.style.opacity = linkP;
        direct.glow.style.opacity = 0.30 * linkP;
        if (linkP > 0) {
          const L = direct.core.getTotalLength();
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
        direct.core.style.opacity = linkP; direct.glow.style.opacity = 0.30 * linkP;
        if (linkP > 0) { const L = direct.core.getTotalLength();
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
          const la = halfA.getTotalLength(), lb = halfB.getTotalLength();
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
        relayPath.setAttribute("opacity", rp2); relayGlow.setAttribute("opacity", 0.26 * rp2);
        if (rp2 > 0) { const L = relayPath.getTotalLength();
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
    };
  }

  /* ====================================================================== *
   *  SCENE: cta — self-hosting
   * ====================================================================== */
  function cta(layer, api) {
    const head = anchor(layer, 960, 176, "h2");
    head.textContent = "Want full control? Run your own.";
    const sub = anchor(layer, 960, 248);
    sub.className += " lead";
    sub.style.fontSize = "30px";
    sub.textContent = "the public server is free — and needs no setup";

    // beat 1: the public server itself, sitting in the same slot the terminal
    // later takes over — so the frame is never empty and the hand-off reads.
    const pub = anchor(layer, 960, 470, "card hub");
    pub.innerHTML = `<div class="glyph">${SERVER}</div><div class="name">fsend.alzina.dev</div>
      <div class="tag">free · no setup</div>`;

    const term = anchor(layer, 960, 470, "term");
    term.style.width = "760px";
    term.innerHTML = `<div class="bar"><i class="r"></i><i class="y"></i><i class="g"></i>
      <span class="name">your server — zsh</span></div>
      <div class="body">
        <div class="ln" id="l1">$ fsend server</div>
        <div class="ln ok" id="l2">→ listening on :443</div>
      </div>`;
    const l2 = term.querySelector("#l2");

    const cards = [
      ["Docker image", "poliuscorp/fsend", DOCKER],
      ["Self-hosting guide", "docs/self-hosting.md", BOOK],
    ].map(([label, value, icon], i) => {
      const c = anchor(layer, 960 + (i === 0 ? -270 : 270), 650, "card inforow");
      c.style.width = "480px";
      c.innerHTML = `<div class="ic">${icon}</div><div><div class="ilabel">${label}</div>
        <div class="ivalue mono">${value}</div></div>`;
      return c;
    });
    const foot = anchor(layer, 960, 812, "mono");
    foot.style.fontSize = "26px";
    foot.style.color = "var(--dim)";
    foot.textContent = "github.com/polius/fsend";

    return function (t) {
      reveal(head, ramp(t, -0.62, -0.06));
      reveal(sub, ramp(t, 1.28, 1.98));
      reveal(pub, ramp(t, 1.05, 1.8) * (1 - ramp(t, 3.15, 3.65)), { dy: 20, scale: 0.95 });
      const r0 = api.segStart("cta2");
      reveal(term, ramp(t, r0 + 0.05, r0 + 0.85), { dy: 22 });
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
    const wm = anchor(layer, 960, 268);
    A.wordmark(wm, 140);
    const under = anchor(layer, 960, 356);
    under.innerHTML = `<div class="underline"></div>`;
    const head = anchor(layer, 960, 430);
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
      const r = anchor(layer, 960, 512 + i * 88, "install-row");
      r.style.width = "1320px";
      r.innerHTML = `<span class="os">${os}</span><span class="sep"></span><span class="cmd">${cmd}</span>`;
      return r;
    });

    const words = ["Simple", "Private", "Free"];
    const pills = words.map((w) => {
      const p = anchor(layer, 0, 852, "pill");
      p.style.fontSize = "30px";
      p.textContent = w;
      return p;
    });
    const foot = anchor(layer, 960, 940, "mono");
    foot.style.fontSize = "26px";
    foot.style.color = "var(--dim)";
    foot.textContent = "github.com/polius/fsend";

    return function (t) {
      reveal(wm, ramp(t, -0.62, -0.06));
      const cur = wm.querySelector(".cur");
      if (cur) cur.style.opacity = Math.floor(t * 2) % 2 === 0 ? 1 : 0.05;
      under.querySelector(".underline").style.transform = `scaleX(${A.easeOutExpo(ramp(t, -0.45, 0.35)).toFixed(3)})`;
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
        outer.style.top = "852px";
        reveal(p, ramp(t, at, at + 0.32), { ease: A.easeOutBack, dy: 14, scale: 0.88, blur: 5 });
        x += widths[i] + gap;
      });
      // settle: after the sign-off, dim the install block and let the end hold
      const settle = ramp(t, api.on("outro2", "free") + 0.8, api.on("outro2", "free") + 2.0);
      [head, under, foot, ...rows].forEach((n) => { n.style.opacity = (1 - 0.4 * settle).toFixed(3); });
    };
  }

  /* ------------------------------------------------------------------ */
  const LOCK = `<svg viewBox="0 0 24 24" width="22" height="22" fill="none" aria-hidden="true">
    <rect x="5" y="10.5" width="14" height="10" rx="2.6" fill="currentColor"/>
    <path d="M8 10.5V8a4 4 0 0 1 8 0v2.5" stroke="currentColor" stroke-width="2.2"/></svg>`;
  const DOCKER = `<svg viewBox="0 0 32 24" width="34" height="26" fill="none" aria-hidden="true">
    <path d="M6 12h4v4H6zM11 12h4v4h-4zM16 12h4v4h-4zM11 7h4v4h-4zM16 7h4v4h-4z" fill="currentColor" opacity="0.85"/>
    <path d="M3 16h26c0 4-3.4 6-8.4 6H11C6.2 22 3 19.7 3 16z" fill="currentColor" opacity="0.5"/></svg>`;
  const BOOK = `<svg viewBox="0 0 24 24" width="26" height="26" fill="none" aria-hidden="true">
    <path d="M4 5.5C4 4.7 4.6 4 5.4 4H11v16H5.4C4.6 20 4 19.3 4 18.5zM20 5.5C20 4.7 19.4 4 18.6 4H13v16h5.6c.8 0 1.4-.7 1.4-1.5z" stroke="currentColor" stroke-width="1.8" fill="none"/></svg>`;

  window.SCENES.brand = brand;
  window.SCENES.cmd = cmd;
  window.SCENES.modes = modes;
  window.SCENES.local = (l, a) => transferScene(l, a, "local");
  window.SCENES.direct = (l, a) => transferScene(l, a, "direct");
  window.SCENES.relay = (l, a) => transferScene(l, a, "relay");
  window.SCENES.cta = cta;
  window.SCENES.outro = outro;
})();
