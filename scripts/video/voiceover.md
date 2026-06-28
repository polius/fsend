# fsend explainer — narration script

Human-readable mirror of the narration. The **source of truth is `SEGMENTS` in
`build_audio.py`** — edit there and regenerate with `./make.sh`; this file is just
the reading copy. Timecodes below are approximate (audio-driven, so they shift
slightly whenever the voice is re-synthesized).

Neural TTS (Kokoro-82M, voice `af_heart` — warm, confident female, speed 0.98),
loudness-normalized to −16 LUFS. Brand name reads as "ef-send".

Total runtime: **~1:31** · 1920×1080 · 30fps · captions ship as a sidecar
`.srt` (off by default — not embedded).

| Time | Scene  | Line |
|------|--------|------|
| 0:00 | brand  | Presenting fsend — a command-line tool that sends files and folders straight from one computer to another. |
| 0:10 | cmd    | You point fsend at a file, and it hands you a short code. |
| 0:15 | cmd    | Your friend runs that code, and the file lands on their machine. |
| 0:20 | modes  | Behind the scenes, fsend finds the best path on its own. First it tries your local network. If that's not possible, it connects directly across the internet. And only if a strict network blocks that, it falls back to an encrypted relay. |
| 0:36 | local  | Here's how each one works. On the same local network, fsend finds the other device and connects directly — no server involved. |
| 0:44 | local  | The server is never even contacted. It can be completely offline. |
| 0:49 | direct | If the two devices aren't on the same local network, fsend connects them directly, across the internet. The server only helps them find each other. |
| 0:59 | direct | Then it steps aside. Your bytes flow straight, peer to peer, across the internet. |
| 1:04 | relay  | And if a direct connection isn't possible either, fsend falls back to a relay. The server forwards the data between them. |
| 1:12 | relay  | But only as sealed, encrypted packets. |
| 1:15 | cta    | The free public server needs zero setup. |
| 1:18 | cta    | But if you want full control, run your own with a single command. Or just pull the Docker image. |
| 1:24 | outro  | fsend. |
| 1:26 | outro  | Simple, private and free. |

## Narrative arc
Brand reveal → one-command demo (Sender / Receiver) → **"How fsend works"** overview
(a local → direct → relay fallback waterfall) → the three transfer modes in detail
(LOCAL / DIRECT / RELAY, each shown end-to-end encrypted) → self-host CTA (with Docker)
→ install outro.

## Accuracy guardrails (kept truthful)
- **local**: "the server is never contacted" — true for mDNS-discovered LAN transfers.
- **direct**: the server only does pairing (signaling + NAT hole-punch), then bytes go
  peer-to-peer.
- **relay**: the relay forwards only ciphertext (TLS 1.3/QUIC terminates at the peers,
  authenticated by the share code via PAKE). We claim it can't read the **contents** —
  we do NOT claim it can't see the encrypted byte volume (which it can).
- **brand pills**: peer-to-peer, end-to-end encrypted, open source — all accurate.

## Re-rendering
- Edit `SEGMENTS` in `build_audio.py`; regenerate with `./make.sh`.
- Swap voice/pace in `synth_kokoro.py` (`VOICE` / `SPEED`).
- Gotcha: a few intra-segment animation onsets are hand-tuned to the current voice —
  the mode-card reveals (`appear_t` in `sc_modes`) and the outro pills (`fracs` in
  `sc_outro`). Re-check these if you change the `modes1` / `outro2` wording or the voice.
