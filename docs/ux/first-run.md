# First-run experience

**Status:** Locked
**Date:** 2026-06-03
**Scope:** What happens the very first time a user runs `fsend` after
installing.

---

## Design principle

Do not get in the way. Most users installed fsend because someone told
them "run this command." The first invocation should succeed at that
command, not pop up onboarding flows or terms-of-service prompts.

But there are exactly two things a first-time invocation must do that
later invocations don't:

1. **Briefly disclose what fsend talks to** (the rendezvous server).
   One line, dismissable.
2. **Ensure the config directory exists** with the right permissions.

That's it. No interactive setup, no welcome banner, no account creation.

## The state machine

A user is "first-run" if and only if `~/.config/fsend/config.json` does
not exist (or is missing the `first_run_completed` field).

After first-run, the field is set to `true` and the disclosure is never
shown again.

## What "first-run" looks like

User just installed fsend. Someone gave them a code. They run:

```
$ fsend abc-defgh-jkm

  Welcome to fsend (first run).

  • To pair you with the other person, fsend will contact:
      fs.alzina.dev    (the default rendezvous server)
    To use your own server later:  fsend --connect <host:port>

  Privacy: https://github.com/polius/fsend/blob/main/docs/security/privacy.md

  ──────────────────────────────────────────────

  Receiving from Pol's MacBook
  ...
```

The disclosure block is prepended to the normal output.
Below the `──────` it's a normal first-time receive. The user is not
required to confirm or press any key — they just keep going.

After this run completes (success or failure), the config file is
written and the disclosure never appears again.

## First-run on the send side

Symmetric. The disclosure prepends to the normal send UX:

```
$ fsend report.pdf

  Welcome to fsend (first run).

  • To pair you with the other person, fsend will contact:
      fs.alzina.dev    (the default rendezvous server)
    To use your own server later:  fsend --connect <host:port>

  Privacy: https://github.com/polius/fsend/blob/main/docs/security/privacy.md

  ──────────────────────────────────────────────

  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm
  ...
```

## First-run when called with no args (`fsend`)

The help text already serves as onboarding for this case. The disclosure
block is *not* added to `--help`; we keep that output stable. But after
help is shown:

```
... (help text) ...

This is your first run. fsend uses fs.alzina.dev for pairing by default.
See: https://github.com/polius/fsend/blob/main/docs/security/privacy.md
```

Appended as one line. After the first run, even this footer goes away.

## First-run when `--quiet` is set

The disclosure is suppressed in `--quiet` mode (consistency: `--quiet`
suppresses everything non-error). The `first_run_completed` field is
still set after the run, so future invocations behave normally.

## What we explicitly do NOT do on first run

- **No interactive prompts.** No "do you accept the privacy policy?",
  no "would you like to enable telemetry?", no "let us know how you
  installed fsend." Friction kills the moment.
- **No license / EULA screen.** It's open source, MIT-licensed. The
  license file in the repo is the license; nothing else to "accept."
- **No account creation.** Ever.
- **No "send your first file!" tutorial.** The user already has a use
  case; we don't need to invent one.
- **No notification permission prompts** (we don't use OS
  notifications).
- **No telemetry opt-in/opt-out prompt.** There's no telemetry; nothing
  to opt into.

## The `--first-run` flag (post-v1 dev convenience)

For testing the first-run experience without nuking the config:

```bash
fsend --first-run ...    # forces the first-run disclosure to appear
```

Not advertised in help. Useful only for developers / docs screenshots.

## Why the disclosure exists at all

Even though there's no legal requirement (no PII collected, MIT license),
the disclosure is the right thing to do because:

1. **Trust gets built once.** A user who sees "fsend will contact
   fs.alzina.dev" understands the architecture immediately. A user who
   later discovers it via `tcpdump` and feels surprised is the user who
   doesn't recommend fsend to others.

2. **It's discoverable later via help.** But many users never read
   `--help`. First-run is the one moment we're guaranteed to have their
   attention.

3. **It's brief enough not to be friction.** No interaction.
   Skim-able. Different from the typical 30-line EULA wall.

4. **It surfaces the one real network call.** The rendezvous server.
   If the user objects, they have an immediate path to change it
   (`--connect`).

## Config file written on first run

```json
{
  "schema_version": 1,
  "first_run_completed": true,
  "first_run_at": "2026-06-03T14:22:11Z",
  "server": null,
  "server_password": null
}
```

`server: null` means "use compiled-in default." Saving `null` rather
than the literal default lets us change the compiled-in default later
without leaving users pinned to the old one.

## When the user runs the second time

Just the normal output. No disclosure, no fanfare. Same as the rest of
the user's life with fsend. The first-run moment is meant to be
forgettable in a good way.
