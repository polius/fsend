# fsend

Truly peer-to-peer file transfer over QUIC, with NAT hole-punching and
end-to-end encryption authenticated from a short shared code.

```bash
# Send
fsend report.pdf

# Receive (on the other machine)
fsend abc-defgh-jkm
```

## Install

```bash
curl -fsSL https://fs.alzina.dev/install.sh | sh
```

Or download a release binary from [the releases page](https://github.com/polius/fsend/releases).

## How it differs from croc

fsend goes **direct peer-to-peer** for the dominant cross-NAT case, where
croc always relays. See [PROJECT_SPEC.md](PROJECT_SPEC.md) for the full
design rationale.

- QUIC over a single UDP port (vs croc's TCP port range)
- ICE hole-punching with `pion/ice` (vs croc's no NAT traversal)
- SPAKE2 (RFC 9382) channel-bound to TLS 1.3 (X25519 + ML-KEM-768 hybrid)
- BLAKE3 chunk-verified streaming with resume
- Auto-detect compression (zstd when it helps, raw when it doesn't)

## Self-hosting

The default rendezvous server at `fs.alzina.dev` is best-effort. To run
your own, see [`deploy/compose/`](deploy/compose/).

```bash
export FSEND_DOMAIN=fs.example.com
docker compose -f deploy/compose/docker-compose.yml up -d
fsend --connect fs.example.com:443    # on each client
```

## Documentation

- [Project spec](PROJECT_SPEC.md) — architecture, decisions, UX
- [Threat model](docs/security/threat-model.md) — what we defend against
- [Privacy](docs/security/privacy.md) — no telemetry, no logs

## License

MIT
