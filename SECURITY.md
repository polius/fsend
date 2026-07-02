# Security Policy

## Reporting a vulnerability

Please do **not** open a public issue for security problems — that
publishes the bug before users can update.

Report it privately instead: **[Security and quality tab → Report a
vulnerability](https://github.com/polius/fsend/security/advisories/new)**.
The report opens a private advisory thread visible only to you and the
maintainer.

Include what you can: the affected command or component, reproduction
steps, and impact. You'll get an acknowledgment within 72 hours.
Reporters are credited in the release notes unless they prefer
otherwise. There is no bug bounty.

## Supported versions

Only the latest release is supported. fsend is a single self-updating
binary — update with `fsend --update`.

## Scope

In scope: the fsend CLI, the wire protocol, the pairing server and
relay, and the install scripts. Out of scope: vulnerabilities in
third-party dependencies (report those upstream) and denial of service
against your own self-hosted instance.

For the threat model and cryptographic design, see
[docs/security.md](docs/security.md).
