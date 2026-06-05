# Privacy policy (and the no-telemetry promise)

**Status:** Locked
**Date:** 2026-06-03
**Scope:** What data fsend (the CLI) collects, what fs.alzina.dev (the
default relay) sees and retains, what users can verify themselves.

This document is intended to be readable by non-technical users; the
README links to it under "Privacy."

---

## Plain-language summary

- **The fsend CLI on your computer collects no telemetry.** It does not
  phone home, count usage, or report anything to us.
- **The CLI makes no network calls outside of a transfer.** The only
  traffic it generates is the encrypted transfer itself.
- **`fs.alzina.dev` (the default relay) keeps no logs.** While a
  transfer is in progress, the server holds the minimum state needed to
  pair you with the other peer and forward UDP datagrams. That state is
  held in memory only and discarded as soon as your session ends. It
  is never written to disk.
- **`fs.alzina.dev` never sees the content of your files** (end-to-end
  encryption between peers; the relay is architecturally incapable of
  reading payloads).
- **If you self-host your own fsend-server**, the same zero-log default
  applies — unless you explicitly enable logging via your own setup.

## What the CLI collects: nothing

`fsend` (the command-line tool) makes exactly one kind of network call:

1. **Transfer traffic** to/from the other peer (encrypted end-to-end;
   we couldn't read it even if we wanted to), via the rendezvous server
   for pairing

That is the complete list. No analytics endpoint. No crash reporting
service. No usage counters. No version checks. The binary contains no
third-party telemetry SDKs.

You can verify this:
- Source code is open; grep it for HTTP endpoints
- `tcpdump -n` while running fsend — you'll see the rendezvous server
  contact and the peer connection, and nothing else
- Builds are reproducible (see "Reproducibility" below) — you can build
  from source and verify the binary you downloaded matches

## What fs.alzina.dev sees (and forgets)

For every transfer that uses `fs.alzina.dev` (i.e., every transfer that
does not happen over LAN-only mDNS), the server temporarily holds — **in
memory only, never written to disk** — the minimum state needed to do
its job:

| In-memory state | Why | When it's discarded |
|---|---|---|
| Source IP address | NAT reflection (returned to you) + in-memory rate-limit counters | Immediately when the session ends or times out |
| Session code | To pair the two peers | When pairing completes (server marks session done) |
| Session ID (random ULID) | Internal session demux | Same |
| Bytes relayed (when fallback path used) | Enforcing per-session caps in flight | Counter dies with the session |
| Peer addresses for the relay allocation | UDP demux | When the allocation expires (idle timeout) |

The server **does not write logs to disk** in its default configuration.
Process stdout (where Go programs write logs by default) is set to log
level `error` only, which under normal operation produces no output. The
Docker container has no mounted log volume.

What we never see (regardless of in-memory or on-disk):
- File names
- File content
- Hostnames either peer announces (received over E2E TLS, never visible
  to the server)
- File sizes (we see total relayed bytes, not per-file)
- Code passwords (`--pass` value) — these are HMAC'd against the TLS
  session key and never reach the server in any form

We cannot record the things we cannot see. The relay is architecturally
incapable of reading file content because TLS terminates at the peers,
not at us. This isn't a policy promise — it's a structural property of
the design (see [threat-model.md](threat-model.md)).

## Retention

**Zero.** Session state lives in memory while the session is active
(seconds to minutes) and is freed immediately when the session ends or
its idle-timeout fires. A server restart wipes everything that was in
flight.

Because nothing is written to disk:
- There are no logs to rotate
- There are no backups to manage
- There is nothing to subpoena that survives the session

If a user reports a problem, debugging happens with whatever transient
in-memory counters are visible at that moment, or by reproducing the
issue. We do not look up "what happened with session X last Tuesday" —
that state is gone.

## Disclosure to third parties

We cannot provide what we do not have. There are no logs, no archives,
no backups, no historical session records. Any legal request for
historical user data will be met with a truthful "we do not retain that
data" response.

The only data theoretically obtainable from a live server would be the
state of in-memory sessions *right now* — which would require physical
or root access to the running machine and would only capture sessions
active at that instant. We do not voluntarily provide that.

For self-hosted servers we don't operate, we have nothing whatsoever.

## Telemetry decisions you might wonder about

**"Will you add opt-in usage analytics later?"** Only if there's a
specific question we can't answer without it. Not for "growth metrics."
If we ever do, it will be: explicitly opt-in (off by default), clearly
documented, and you can audit what we send because the code is open.

**"Will you add crash reporting?"** No. Errors stay on the user's machine.
Users who want to report a bug send us the logs manually (the CLI has a
`--debug` mode that produces useful output).

**"Will you ever add accounts / authentication?"** Not for fsend. The
whole point is account-less code-based pairing. If we later want
authenticated features, they live in a separate product (filesync.app).

## Reproducibility

To let you verify the binary you downloaded matches the source code:

- GitHub Actions builds binaries with `-trimpath -ldflags="-w -s
  -buildid="` and fixed CGO settings
- The release pipeline publishes:
  - Static binaries per OS/arch
  - SHA-256 checksums in a signed `checksums.txt`
  - `cosign` signatures (transparency log entry in Rekor)
- To verify locally:
  ```bash
  git clone https://github.com/polius/fsend
  cd fsend
  git checkout v0.1.0
  ./scripts/repro-build.sh   # produces ./dist/fsend-<os>-<arch>
  sha256sum dist/fsend-linux-amd64   # should match the published checksum
  ```

If your locally-built binary's SHA differs from the published one, that's
a security event — report at security@alzina.dev.

## Self-hosters

If you run your own `fsend-server` via the reference Docker compose:

- Your own server sees the same transient in-memory session state
  described above (for your users), and by default writes nothing to
  disk — same as ours.
- If you opt into disk logging (by setting `FSEND_LOG_LEVEL=debug` or
  configuring your container runtime to capture stdout), retention is
  whatever you configure. That's your choice and your responsibility.
- You are the data controller for your users; if you operate in the EU,
  GDPR responsibilities are yours (we're not your processor).
- The reference compose stack does NOT send anything to alzina.dev.

## Where this document lives

- Canonical: this file (`docs/security/privacy.md`) in the repo
- Web: `https://fs.alzina.dev/privacy` (a copy of this file rendered to HTML)
- The CLI's `--help` includes a "Privacy: https://github.com/polius/fsend/blob/main/docs/security/privacy.md"
  line

If we ever change what we collect or retain, we will:
1. Update this file with a dated changelog entry at the top
2. Bump the CLI's version
3. Print a notice to stderr the first time the new version runs

Honest, narrow, auditable. That's the goal.
