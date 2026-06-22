# README demo

This directory generates the animated demo at the top of the repo README
(`docs/assets/demo.webp`) — a scripted, fully local fsend transfer shown as two
terminals (sender + receiver) side by side.

Everything runs **locally and offline**: a loopback-only `fsend server`, a
throwaway config/home, and a `/dev/urandom` payload. Nothing touches your real
fsend config, and no real network or pairing server is involved.

This file is the complete runbook: with it, the demo can be regenerated from
scratch with no other context.

---

## TL;DR — regenerate

```bash
# 1. one-time dependencies
brew install vhs tmux ffmpeg webp        # vhs pulls in ttyd + a headless Chrome
brew install --cask font-jetbrains-mono  # the wordmark font used in the demo

# 2. regenerate docs/assets/demo.webp
scripts/demo/record.sh
```

`record.sh` is the single entry point. It builds fsend, renders the recording,
rounds the corners, and writes `docs/assets/demo.webp`. Re-run it any time the
CLI output or styling changes.

> Note for headless/CI runs: VHS drives a headless Chrome + a local `ttyd`
> websocket. If recording fails with `ERR_CONNECTION_REFUSED`, the sandbox is
> blocking localhost — run it with normal network access.

---

## The files

| file | role |
|------|------|
| `demo.sh`    | Sets up the isolated session and **drives** the two terminals (the "director"). |
| `demo.tape`  | The [VHS](https://github.com/charmbracelet/vhs) script: terminal size/style + what to record. Renders a 2x **mp4** intermediate. |
| `record.sh`  | Orchestrates: build → VHS → round corners → encode the final **WebP**. |
| `README.md`  | This runbook. |

Output: `docs/assets/demo.webp`, embedded in the repo README via
`<img src="docs/assets/demo.webp" width="820">`.

---

## The pipeline, end to end

```
record.sh
  ├─ go build → /tmp/fsend-demo/fsend
  ├─ vhs demo.tape
  │     └─ demo.sh setup   (off-camera: server, panes, session)
  │     └─ demo.sh play    (attach; the director types the transfer)
  │     └─ writes /tmp/fsend-demo/demo.mp4   (2x, 25 fps, h264)
  ├─ ffmpeg: build an anti-aliased rounded-rect alpha mask (sized to the mp4)
  ├─ ffmpeg: alphamerge the mask into every frame → RGBA PNGs
  └─ img2webp: assemble the animated WebP, with a long delay on the last frame
        └─ docs/assets/demo.webp
```

### 1. `demo.sh setup` — the stage

- Builds fsend (or reuses `$FSEND_BIN`) and starts a loopback `fsend server`
  with an isolated `XDG_CONFIG_HOME` and `HOME` under `/tmp/fsend-demo`. Points
  the (isolated) client config at the local server with `--connect`.
- Writes a non-compressible `report.pdf` from `/dev/urandom`, so the transfer
  line shows a real size/throughput (a text file would be zstd-compressed and
  look implausibly fast/small).
- Lays out a **two equal `tmux` panes** (`even-vertical`): sender on top,
  receiver below. The divider is hidden (border colour = background) so the gap
  reads as clean whitespace; each pane prints a coral card header
  (`▌ SENDER · your laptop` / `▌ RECEIVER · their laptop`).
- A small `fsend` **shim** on the panes' `PATH` injects `--mode=direct` and a
  neutral `--name`. This pins the transfer to the direct P2P path so the
  recording is deterministic and free of environment-specific LAN-discovery
  notices, and avoids leaking the real hostname — while the on-screen command
  the viewer sees stays a plain `fsend …`.

### 2. `demo.sh play` — the director

Attaches the session (this is what VHS records) and runs a background
**director** that puppeteers both panes via `tmux send-keys`:

1. waits until a client (VHS) has attached, so nothing is typed off-camera;
2. types `fsend report.pdf` in the sender, char by char;
3. **scrapes the dynamically generated share code** out of the sender pane
   (`tmux capture-pane`) and types `fsend <code>` in the receiver;
4. answers the `Save to ~/Downloads? [Y/n]` prompt;
5. holds briefly (the real end-of-loop pause is added later in `record.sh`).

The pauses between steps (in the `director` function) are deliberately spaced so
a viewer can read each beat — especially the share-code instruction and the
incoming-transfer prompt.

### 3. `demo.tape` — VHS

Sizes and styles the terminal at **2x** (JetBrains Mono, macOS window bar,
rounded inner window, coral frame), then:

```
Hide                              # set up + attach off-camera
Type "scripts/demo/demo.sh setup" Enter ; Sleep 3s
Type "scripts/demo/demo.sh play"  Enter ; Sleep 800ms
Show                              # reveal the laid-out, attached panes
Sleep 12s                         # record the director's choreography
```

Output is `/tmp/fsend-demo/demo.mp4` (VHS can't emit WebP, and an mp4
intermediate is full-colour and lossless enough to post-process).

### 4. `record.sh` — round + encode

VHS can only round the *inner* window, so the *outer* coral corners are rounded
in post:

- An **anti-aliased** rounded-rectangle alpha mask is generated once with
  ffmpeg `geq` (a +0.5px ramp at the radius boundary = smooth edges), sized to
  the actual mp4 (so tape geometry changes just work).
- The mask is `alphamerge`d into every frame → RGBA PNGs.
- `img2webp` assembles the animated WebP. Every frame gets `FRAME_MS` (40 ms =
  25 fps) except the **last**, which gets `END_HOLD_MS` (~2.8 s) so the loop
  pauses on the completed transfer before restarting.

---

## Why these choices (so they aren't re-litigated)

- **WebP, not GIF.** GIF is 256-colour (visible banding on text + the coral) and
  its transparency is 1-bit (jagged rounded corners). WebP is full-colour with
  smooth 8-bit alpha, and smaller.
- **Rendered at 2x.** The README displays it at `width="820"`, so a 1x (~1040px)
  source looks soft/pixelated on retina. 2x (2080px) stays crisp.
- **`--mode=direct` via a PATH shim.** Without it the transfer races LAN
  discovery, which can print a `Local-network discovery unavailable` notice in
  headless renders and is non-deterministic. The shim keeps the visible command
  clean.
- **Two equal panes (`even-vertical`), not a 3-pane spacer.** Pane sizes get
  rescaled when VHS attaches at its own size; a fixed spacer split starved the
  receiver and dropped its header. Equal halves survive the rescale and both
  headers always fit.
- **`BASH_SILENCE_DEPRECATION_WARNING=1`** is exported before the panes start,
  or macOS bash prints "the default interactive shell is now zsh" into them.
- **End pause via a last-frame delay,** not a long recorded tail — keeps the
  file small (no tail of identical frames).
- **bash 3.2 compatible.** macOS ships bash 3.2: no `mapfile`, no negative array
  indices (`${a[-1]}`). Keep the scripts portable.

---

## Preview without recording

Watch the choreography live in your own terminal (no file produced):

```bash
scripts/demo/demo.sh
```

`Ctrl-b d` to detach, or just close the terminal — the throwaway server and
tmux session are cleaned up on exit.

---

## Tuning knobs

| want to change | where |
|----------------|-------|
| Pacing / the share-code handoff | `director()` in `demo.sh` (the `sleep`s) |
| Pane labels, colours, layout | `do_setup()` in `demo.sh` |
| Terminal size / theme / font / window bar | `demo.tape` (geometry is **2x** — keep it a clean doubling of the 1x design noted at the top of the tape) |
| macOS button size | `Set WindowBarSize` in `demo.tape` |
| Recording length | `Sleep` in `demo.tape` (end shortly after "Saved") |
| Outer corner radius | `RADIUS` in `record.sh` (= 2 × (tape `Margin` + `BorderRadius`)) |
| End-of-loop pause | `END_HOLD_MS` in `record.sh` |
| WebP quality / size | `img2webp -q` in `record.sh` (lower = smaller) |

---

## Troubleshooting

- **`record.sh` fails after VHS, no "wrote …" line.** A post step failed under
  `set -e`. Common causes: mask/frame size mismatch (the mask is sized from the
  mp4 — re-check if you hand-edited dimensions), or a bash-4-ism slipping into
  the scripts (this must run on bash 3.2).
- **Headers scroll off the top.** A pane is shorter than its content. Increase
  the tape `Height`, or check the `even-vertical` split in `do_setup`.
- **`ERR_CONNECTION_REFUSED` / `could not open ttyd`.** VHS's headless Chrome
  can't reach the local `ttyd` — run outside a network sandbox.
- **Transfer shows a LAN-discovery notice or the wrong path.** Confirm the
  `fsend` shim (with `--mode=direct`) is on the panes' `PATH`.
