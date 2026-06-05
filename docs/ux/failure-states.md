# Failure-state UX

**Status:** Locked
**Date:** 2026-06-03
**Scope:** What the terminal looks like for each failure mode mid-transfer
or pre-transfer. Companion to `help-text.md` (which has the error messages)
— this doc covers the *visual* / spinner / progress-bar behavior.

---

## Design principle

A failure should never leave the screen in an ambiguous state. The user
must know:
1. Whether they should retry, change something, or give up
2. Where to look for more detail
3. That it's truly over (no lingering spinner / progress bar)

## Pre-transfer failures (before any bytes flow)

### Server unreachable (E001)

```
  Sending report.pdf  (4.2 MB)

  ⠋ Connecting to fs.alzina.dev…

  ────────  10 seconds later  ────────

  ✗ Could not reach rendezvous server fs.alzina.dev:443 (timeout after 10s).
    Check your internet connection, or use a different server:
      fsend --connect <host:port>
```

The spinner exits cleanly (`\r` + clear-line + then the error block).
Cursor returns to a new prompt; no leftover artifacts.

### Code not found (receiver side, E002)

```
$ fsend abc-defgh-jkm

  ⠋ Connecting to fs.alzina.dev…
  ⠋ Looking up code abc-defgh-jkm…

  ✗ Code "abc-defgh-jkm" was not found.
    Ask the sender to re-run their command — codes expire after 60 seconds.
```

### Invalid code format (E004)

```
$ fsend xyz123

  ✗ Invalid code format. Codes look like: abc-defgh-jkm
```

No spinner — fails immediately at argument parse time.

### Receiver declines (E006), sender's view

```
  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm

  On the other machine, run:
      fsend abc-defgh-jkm

  ──────────────────────────────────────────────

  ⠋ Waiting for receiver…

  ────────  Bob types fsend, sees prompt, says no  ────────

  ✗ Receiver declined the transfer.
```

The artifact block (code + receive command) stays on screen; only the
status line below the second `──` is replaced with the failure message.

### Receiver prompt timeout (E007), receiver's view

```
$ fsend abc-defgh-jkm

  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

  Save to current directory? [Y/n]: _

  ────────  30 seconds with no input  ────────

  ✗ No response received within 30 seconds. Transfer aborted.
```

### Wrong password (E005), receiver's view

```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)  🔒 Password required

  ──────────────────────────────────────────────

  Enter password: ***

  ✗ Wrong password.
    Double-check with the sender and run the command again.
```

The typed password is masked (`***`), never echoed. After failure, the
user re-runs from scratch — no in-place retry (see threat-model on
"discourage interactive guessing").

### NAT traversal + relay both fail (E014)

```
  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm  →  Sarah's MacBook

  ──────────────────────────────────────────────

  ⠋ Establishing connection (trying direct path…)
  ⠋ Establishing connection (trying relay fallback…)

  ✗ Could not connect to the other peer, even via the relay.
    This usually means one of:
      - The relay server is unreachable from your network
      - The other peer's connection dropped
      - Your firewall blocks UDP traffic
    Try: fsend --connect <different-server> or run with --debug for details.
```

The two attempts each get their own spinner line, replaced as we progress.
Both go to the error message in place.

## Mid-transfer failures (during the actual byte flow)

### Network drops mid-transfer (connection lost)

```
  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm  →  Sarah's MacBook

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  ▕████████████████░░░░░░░░░░░░░░░░░░░░░░░░▏  42%   1.8 MB/s   ETA 1s

  ────────  Wi-Fi drops  ────────

  ✗ Connection to receiver lost at 42% (1.8 MB sent).
    The receiver has 1.8 MB saved and can resume — just re-run on both sides:
      fsend report.pdf
    (The receiver picks up from their .fsend-partial sidecar.)
```

Critical UX detail: **tell the user the transfer is resumable AND tell
them how.** Resume in fsend works across invocations because the receiver
keeps a `.fsend-partial` sidecar keyed by destination filename — see
[resume.md](../decisions/resume.md). The error message embeds the recipe.

### Disk full on receiver (E008)

Receiver's view:
```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)  →  ./

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  ▕████████░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░░▏  21%   1.8 MB/s   ETA 4s

  ✗ Not enough disk space (need 4.2 MB, 2.1 MB available in ./).
    Free up space or use --out <dir> to save somewhere else.
    Partial download (900 KB) has been deleted.
```

Sender's view:
```
  ✗ Receiver ran out of disk space. Transfer aborted.
```

### File hash mismatch at end (E011)

Receiver's view:
```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)  →  ./

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  ▕████████████████████████████████████████▏  100%

  ✗ Transfer completed but the file did not verify correctly.
    This usually means the sender's file changed mid-transfer, or there
    was data corruption. The partial file has been deleted.
    Ask the sender to try again.
```

### Sender Ctrl-C mid-transfer

Sender's view:
```
  Sending report.pdf  (4.2 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm  →  Sarah's MacBook

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  ▕████████████████░░░░░░░░░░░░░░░░░░░░░░░░▏  42%
  ^C
  ✗ Transfer cancelled by sender.
```

Receiver's view (the other side seeing the cancellation):
```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)  →  ./

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  ▕████████████████░░░░░░░░░░░░░░░░░░░░░░░░▏  42%

  ✗ Sender cancelled the transfer.
    Partial download (1.8 MB) saved as report.pdf.fsend-partial — you
    can resume by re-running the same command if the sender re-sends.
```

The Ctrl-C handler sends `ABORT` frame before exiting, so the receiver
gets the clean message rather than a generic "connection lost."

### Receiver Ctrl-C mid-transfer

Mirror of the above; sender sees "Receiver cancelled the transfer" and
exits cleanly.

### Resume happening (the success state for an interrupted earlier
transfer)

Receiver's view:
```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      report.pdf  (4.2 MB)  →  ./

  ──────────────────────────────────────────────

  ↻ Resuming from 1.8 MB (43% already on disk)
  ✓ Direct connection established  (no relay)
  ▕████████████████░░░░░░░░░░░░░░░░░░░░░░░░▏  43%   1.8 MB/s   ETA 1s
```

The `↻ Resuming` line is the key UX moment — users need to know that
nothing's been re-transferred.

## Edge cases that are not errors but warrant a notice

### Partial directory transfer completes with some failures

```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      myproject/  (142 files, 18.4 MB)  →  ./

  ──────────────────────────────────────────────

  ✓ Direct connection established  (no relay)
  Overall:  ▕████████████████████████████████████████▏  100%

  ✓ Received 18.4 MB in 12 s (140 files OK)
  ⚠ 2 files failed:
      myproject/restricted.bin    permission denied
      myproject/.DS_Store         hash verification failed
    Re-run to retry just the failures.
```

Mixed-success output uses `⚠` (warning) rather than `✗` (error), exit
code 0 because *something* succeeded.

### Relay was used (not an error, but worth showing)

In the success case where ICE failed:
```
  ⚠ Relayed via fs.alzina.dev (NAT hole-punch failed)
  ✓ Sent 4.2 MB in 8.1 s  (0.52 MB/s avg)
```

The honest disclosure helps users understand:
- Why it was slower than they expected
- That improving their network (or the other peer's) could give direct
  transfers

## Progress bar behavior

- **Updates ≥10 Hz when active**, but capped at the terminal's refresh
  capability
- **Locks to a single line per file in single-file mode**; the
  `vbauerster/mpb` library handles cursor management
- **In directory mode, shows overall + currently-active file**, scrolls
  completed files out
- **On error**, the bar stays visible (frozen at last position) until
  the error message renders; gives the user the visual "we got this far"
  context

## When the terminal isn't a TTY (piped output)

All the box-drawing characters and ANSI sequences degrade:
- `──────` becomes `------`
- Spinner `⠋` becomes a static `[*]`
- `✓` becomes `[OK]`
- `✗` becomes `[FAIL]`
- `⚠` becomes `[WARN]`
- `↻` becomes `[RESUME]`
- Progress bar becomes periodic plain-text updates: `42% 1.8/4.2 MB`
- No cursor manipulation; just `\n`-separated lines

This means `fsend report.pdf 2>&1 | tee log.txt` produces a readable log
file.

## When `--quiet` is set

- All artifact/status output: suppressed
- Code (on send): printed as a bare line to stdout: `abc-defgh-jkm\n`
- Errors: still printed to stderr (so `2>` captures them)
- Progress: nothing
- Exit code: same as non-quiet

This makes `fsend file --quiet | pbcopy` and `if ! fsend file --quiet
2>err.log; then ...` trivial.

## When `--debug` is set

- Everything from the standard UX, plus
- A `DEBUG:` block appended after any error with the underlying technical
  details
- Verbose logs to stderr throughout
- ICE candidate gathering shown line by line
- Wire-protocol frame types logged
- Useful for issue reports; users include `--debug` output in bug
  templates
