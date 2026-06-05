# Help text + error message catalog

**Status:** Locked
**Date:** 2026-06-03
**Scope:** The exact text output of `fsend --help`, `fsend-server --help`,
and the user-facing error messages for the top failure modes.

These are the most-read pieces of writing in the product. They should
make a new user successful in under a minute and tell an erroring user
exactly what to do next.

---

## `fsend --help` (the CLI)

```
fsend — peer-to-peer file transfer

USAGE
  fsend <file|dir>...              Send (1 or more paths)
  fsend <code>                     Receive (using a code like abc-defgh-jkm)
  fsend -                          Send from stdin
  fsend --text "hello world"       Send a literal string

EXAMPLES
  Send a file:
    fsend report.pdf
    → shows code, waits for receiver

  Receive:
    fsend abc-defgh-jkm

  Send a whole folder:
    fsend ./myproject

  Send with extra password protection:
    fsend report.pdf --pass "shared-secret"

  Send to a different server:
    fsend report.pdf --connect relay.mycompany.com:443

COMMON FLAGS
  --code <code>          Use a specific code (skip random generation)
  --pass <password>      Require the receiver to enter this password
  --out <dir>            Receive into this directory (default: current)
  --yes                  Auto-accept incoming transfers (no prompt)
  --overwrite            Overwrite existing files on receive
  --quiet                Suppress all non-error output
  --help                 Show this help
  --version              Show version

ADVANCED FLAGS
  --connect <host:port> [password]   Set the rendezvous server (persisted)
  --connect default                  Revert to fs.alzina.dev:443
  --connect                          Show current server
  --no-clipboard                     Don't copy code to clipboard
  --no-compress                      Force-disable compression
  --name <string>                    Override hostname shown to peer
  --send / --receive                 Force mode (skip auto-detect)
  --text "<string>"                  Send a literal string
  --uninstall                        Remove fsend and its config

ENVIRONMENT
  FSEND_DEBUG=1                      Verbose logging to stderr

LEARN MORE
  Docs    https://github.com/polius/fsend
  Privacy https://github.com/polius/fsend/blob/main/docs/security/privacy.md
  Issues  https://github.com/polius/fsend/issues
```

**Design notes (locked):**

- **Examples first, flags second.** Most users learn by example; flag
  walls are reference material.
- **Common flags vs Advanced flags.** Two-tier structure prevents the
  full flag list from being intimidating. ~9 flags any user might use
  vs ~10 power-user flags.
- **Single column.** Two-column flag tables look pretty but wrap badly
  in narrow terminals.
- **No ASCII art / logo.** Not in `--help`. Stays clean.
- **Total output: ~50 lines.** Fits in one screen on any modern terminal
  (24+ rows is the minimum we design for).

## `fsend-server --help`

```
fsend-server — rendezvous + relay server for fsend

USAGE
  fsend-server                Run the server (config via env vars)

EXAMPLE
  Standard Docker run (zero-config):
    docker run -p 443:443/udp -p 8080:8080/tcp ghcr.io/polius/fsend-server

  Behind Caddy with auto-TLS (see docker-compose.yml in the repo):
    docker compose up

CONFIGURATION (environment variables — all optional)
  FSEND_HTTP_ADDR                       Default :8080   Signaling HTTP listener
  FSEND_UDP_ADDR                        Default :443    Relay UDP listener
  FSEND_LOG_LEVEL                       Default info    debug | info | warn | error
  FSEND_MAX_RELAY_BYTES_PER_SESSION     Default 100MiB  Per-session byte cap
  FSEND_MAX_SESSIONS_PER_IP             Default 5       Concurrent sessions per IP
  FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN Default 30      Rate limit
  FSEND_SESSION_IDLE_TIMEOUT            Default 60s     Drop idle relay sessions

  Setting any limit to 0 disables that specific check. For IP blocking,
  use your reverse proxy or host firewall — fsend-server itself does not
  maintain a blocklist.

FLAGS
  --help     Show this help
  --version  Show version

LEARN MORE
  Reference compose    https://github.com/polius/fsend/tree/main/deploy/compose
  README               https://github.com/polius/fsend
```

## Error message catalog

Format for every error:

```
✗ <one-line summary of what went wrong>
  <one-line actionable next step>
  [optional second line for context or a link]
```

Color: red for the `✗` line, default for the rest. Always to stderr.
Exit code as listed.

### E001 — Server unreachable

```
✗ Could not reach rendezvous server fs.alzina.dev:443 (timeout after 10s).
  Check your internet connection, or use a different server:
    fsend --connect <host:port>
```
**Exit:** 2 (network error)

### E002 — Code not found / expired

```
✗ Code "abc-defgh-jkm" was not found.
  Ask the sender to re-run their command — codes expire after 60 seconds.
```
**Exit:** 3 (session not found)

### E003 — Code already used (race condition: two people typed it)

```
✗ Code "abc-defgh-jkm" has already been claimed by another receiver.
  Ask the sender to generate a fresh code.
```
**Exit:** 3

### E004 — Receiver typed wrong code

```
✗ Invalid code format. Codes look like: abc-defgh-jkm
```
**Exit:** 4 (bad input)

### E005 — Password mismatch (`--pass`)

On the receiver:
```
✗ Wrong password.
  Double-check with the sender and run the command again.
```
On the sender:
```
✗ Receiver entered the wrong password. Transfer aborted.
```
**Exit:** 5 (auth failure)

### E006 — Receiver declined

On the sender:
```
✗ Receiver declined the transfer.
```
On the receiver: (no error — they made the choice)
**Exit:** 6

### E007 — Receive prompt timed out

On the receiver:
```
✗ No response received within 30 seconds. Transfer aborted.
```
On the sender:
```
✗ Receiver did not respond within 30 seconds. Transfer aborted.
```
**Exit:** 7

### E008 — Disk full on receiver

```
✗ Not enough disk space (need 4.2 MB, 2.1 MB available in ./).
  Free up space or use --out <dir> to save somewhere else.
```
**Exit:** 8

### E009 — File system error (write failed)

```
✗ Could not write to ./report.pdf: permission denied.
  Try --out <dir> to save somewhere writable.
```
**Exit:** 9

### E010 — File system error (read failed)

```
✗ Could not read ./report.pdf: permission denied.
  Check the file permissions and try again.
```
**Exit:** 10

### E011 — File hash verification failed

```
✗ Transfer completed but the file did not verify correctly.
  This usually means the sender's file changed mid-transfer, or there
  was data corruption. The partial file has been deleted.
  Ask the sender to try again.
```
**Exit:** 11 (integrity failure)

### E012 — Path traversal attempt (security: peer tried `../`)

```
✗ Sender tried to write outside the target directory. Transfer rejected.
  This is a security check — please report at:
    https://github.com/polius/fsend/issues/new?label=security
```
**Exit:** 12

### E013 — Target file exists, no --overwrite

```
? report.pdf already exists. Overwrite? [y/N]: _
```
(Interactive prompt, not an error. If user says no:)
```
✗ Transfer cancelled to avoid overwriting report.pdf.
  Use --overwrite to replace existing files.
```
**Exit:** 13

### E014 — NAT traversal failed, relay also failed

```
✗ Could not connect to the other peer, even via the relay.
  This usually means one of:
    - The relay server is unreachable from your network
    - The other peer's connection dropped
    - Your firewall blocks UDP traffic
  Try: fsend --connect <different-server> or run with --debug for details.
```
**Exit:** 14

### E015 — Protocol error (unexpected frame, version mismatch)

```
✗ Protocol error talking to the other peer.
  Both sides must run fsend v0.1.0 or higher.
  Other peer reported: fsend v0.0.5
```
**Exit:** 15

### E016 — Configuration file corrupted

```
⚠ Your config file at ~/.config/fsend/config.json is invalid.
  Falling back to defaults. To reset:
    fsend --connect default
```
**Exit:** 0 (warning only, continues)

### E017 — Rate limit hit

```
✗ Too many attempts from your network — rate limit hit on the server.
  Wait a minute and try again, or use --connect to use a different server.
```
**Exit:** 17

### E018 — Server reports it's shutting down (HTTP 410 Gone)

```
✗ The default server (fs.alzina.dev) has been retired.
  Switch to a different server with:
    fsend --connect <host:port>
  Or self-host: https://github.com/polius/fsend#self-hosting
```
**Exit:** 18

### Catchall — unexpected error

```
✗ Unexpected error: <go error message>
  Please report this at:
    https://github.com/polius/fsend/issues
  Include the output of:  fsend --debug ... (rerun your command with --debug)
```
**Exit:** 99

## Design rules for all error messages (locked)

1. **First line says what failed in user terms**, not "context deadline
   exceeded" or "EOF" — those get hidden behind `--debug`.
2. **Second line gives a concrete action.** "Try X" is better than
   "Something went wrong."
3. **Never blame the user.** "Code not found" not "you typed it wrong."
4. **Never expose internal jargon.** "Rendezvous server" is borderline
   (a normal user might not know that word) — say "server" in error
   messages, save "rendezvous" for docs.
5. **Stable exit codes.** Scripts depend on these. Catalog above is
   locked from v0.1.0 onward; new errors get new codes, never reuse.
6. **`--debug` mode appends the underlying technical error** in a second
   block after the user-facing message, prefixed `DEBUG:`. Lets reporters
   include useful context without polluting the default UX.
