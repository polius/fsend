# Security policy

## Reporting a vulnerability

Email **security@alzina.dev**. Please include:

- A description of the issue
- Steps to reproduce
- The version of fsend (`fsend --version`)
- Your contact details (we will acknowledge within 72 hours)

We follow coordinated disclosure with a 90-day window from acknowledgment.

## Scope

In scope:
- The `fsend` CLI binary and `fsend-server` binary
- The signaling and relay protocols
- The cryptographic constructions (SPAKE2 channel binding, TLS exporter use)
- The reference `docker-compose.yml` and `Caddyfile`

Out of scope:
- Compromised endpoints (we cannot defend against an attacker with code
  execution on the user's machine)
- Issues in upstream dependencies (please report those upstream)
- Denial of service via the public relay at `fs.alzina.dev` (rate-limited
  by design; not a vulnerability)

See [docs/security/threat-model.md](docs/security/threat-model.md) for
the full threat model.
