# Security Policy

## Reporting a vulnerability

Don't open a public issue. Report it privately:
**[Report a vulnerability](https://github.com/polius/fsend/security/advisories/new)**.
Only you and the maintainer can see it.

Include the affected command or component, steps to reproduce, and the
impact. You'll get a reply within 72 hours. Reporters are credited in
the release notes unless they'd rather not be. There's no bug bounty.

## Supported versions

Only the latest release. Update with `fsend --update` (or
`brew upgrade fsend`). To verify a release, see
[Release integrity](docs/security.md#release-integrity).

## Scope

- **In scope:** the CLI, the wire protocol, the pairing server and
  relay, and the install scripts.
- **Out of scope:** bugs in third-party dependencies (report them
  upstream) and denial of service against your own server.

For the threat model, see [docs/security.md](docs/security.md).
