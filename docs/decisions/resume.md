# Decision: Resume (interrupted transfers)

**Status:** Locked
**Date:** 2026-06-03
**Scope:** How a partially-received file is detected, validated, and resumed
when the user re-runs the transfer.

---

## User-visible behavior

1. Receiver starts `fsend abc-defgh-jkm`, transfer is interrupted (network
   drop, Ctrl-C, sender crash).
2. Receiver re-runs `fsend abc-defgh-jkm` (or, more often, the sender
   re-runs with the same code).
3. Receiver sees: `Resuming report.pdf — already have 1.8 MB of 4.2 MB`
4. Sender resumes from the appropriate chunk boundary.
5. Final file passes BLAKE3 root verification.

If verification fails at the end (e.g., the user manually edited the partial
file in the interim, or the sender is sending a *different* file with the
same name), the partial is deleted and the transfer restarts from chunk 0.

## On-disk format

For each in-progress receive, the receiver creates a sidecar file:

```
<target-filename>.fsend-partial
```

For example, while receiving `report.pdf`, the receiver holds:
- `report.pdf` — the partial file content, chunks written in order
- `report.pdf.fsend-partial` — the resume metadata

Sidecar contents (JSON, since it's tiny and human-debuggable):

```json
{
  "schema_version": 1,
  "filename": "report.pdf",
  "expected_size": 4404019,
  "expected_blake3_root": "9a8b7c...",
  "bytes_written": 1887436,
  "chunks_complete": 1799,
  "chunk_size": 1048576,
  "started_at": "2026-06-03T14:22:11Z",
  "last_updated_at": "2026-06-03T14:22:47Z",
  "sender_hostname": "Pol's MacBook",
  "sender_fingerprint": "84.27.123.45"
}
```

`bytes_written` and `chunks_complete` are redundant but cheap and useful for
quick consistency checks.

## Resume handshake

After the sender's `FILE_INFO` frame for a given file, the receiver checks
for a matching `.fsend-partial`:

```
   SENDER                              RECEIVER
   ──────                              ────────
   FILE_INFO{
     RelativePath: "report.pdf",
     Size: 4404019,
     Blake3Root: 0x9a8b7c...,
     ...
   } ────────────────────────────────►
                                       Look for "report.pdf.fsend-partial"
                                       Validate:
                                         - schema_version == 1
                                         - filename matches
                                         - expected_size matches
                                         - expected_blake3_root matches
                                         - bytes_written == chunks_complete × chunk_size
                                           (or final-chunk-aware equivalent)
                                         - target file size on disk == bytes_written
                                       If all valid:
                                       ◄──── FILE_ACCEPT{
                                                Action: RESUME,
                                                ResumeOffset: bytes_written
                                              }
                                       Else (mismatch or no sidecar):
                                       ◄──── FILE_ACCEPT{
                                                Action: ACCEPT_FULL,
                                                ResumeOffset: 0
                                              }
   Begin sending chunks starting at
   chunk_index = ResumeOffset / chunk_size
```

## Validation rules (locked)

A partial is **only** resumable when **all** of these match:

| Field | Source | Why |
|---|---|---|
| `expected_blake3_root` | Sender's current `FileInfo.Blake3Root` | If the sender's file has changed since last attempt, the resumed bytes would not verify at the end |
| `expected_size` | Sender's current `FileInfo.Size` | Sanity; can't resume if the file shrank or grew |
| `filename` | Target path the user is receiving to | Defensive — guards against a different file being received into the same partial |
| `bytes_written` consistent with on-disk size | Filesystem | Catches manual edits or truncation between runs |
| `schema_version == 1` | Sidecar JSON | Forward-compat |

If **any** check fails:
- Delete the existing partial file and sidecar.
- Send `FILE_ACCEPT{Action: ACCEPT_FULL, ResumeOffset: 0}`.
- Start fresh.
- Log a debug-level message explaining which check failed (helps debugging
  but doesn't bother the user — most resume failures are "the sender
  re-generated the file" which is unsurprising).

## Write ordering (durability)

For each chunk written to disk:

1. Write chunk payload to target file at `chunks_complete × chunk_size` offset.
2. `fsync(target_file)` — durable on disk.
3. Update sidecar JSON in memory.
4. Atomically write sidecar via `write-then-rename` (write to
   `report.pdf.fsend-partial.tmp`, then `rename` to `report.pdf.fsend-partial`).
5. `fsync(sidecar)` — durable.

This ordering guarantees that:
- If we crash between step 1 and step 3: on next run, sidecar says we have
  N chunks but file is N+1 chunks long → validation catches the inconsistency,
  restarts from 0. **Safe**, just wastes the work since last sidecar update.
- If we crash between step 3 and step 4: on next run, sidecar says we have
  N chunks (old), file has N+1 chunks → same as above. **Safe.**
- If we never crash: sidecar always reflects bytes durably on disk.

**Sync frequency:** fsync on every chunk is expensive. To balance durability
vs throughput, we fsync the sidecar **every 16 MiB** of received data (not
every chunk), and one final fsync at end-of-file. Lost work on crash is
bounded to ~16 MiB.

## Cleanup

Successful transfer end:
1. Verify the BLAKE3 root of the completed target file matches
   `expected_blake3_root`. If mismatch → delete file + sidecar, error to user.
2. If match → delete the `.fsend-partial` sidecar. Target file remains.

Failed/aborted transfer:
- Sidecar and partial target file remain on disk so a re-run can resume.
- User can manually delete `<filename>.fsend-partial` to force a restart.

## Sender-side handling

The sender doesn't keep resume state — the receiver is the source of truth.
On resumption:
- Sender opens the source file fresh.
- Seeks to `ResumeOffset` bytes (`Seek(ResumeOffset, io.SeekStart)`).
- Reads + sends chunks starting at `chunk_index = ResumeOffset / chunk_size`.
- The chunk indices in the data-stream frames reflect this — they continue
  from where they left off, they don't restart from 0.

## Directory transfers

For directory transfers, each file gets its own `.fsend-partial` sidecar.
Resuming a directory transfer is "resume each file independently":
- Completed files: skipped (sender sees the file already exists at correct
  size + hash via the FILE_ACCEPT response action `SKIP`).
- Partially-received files: resumed via this protocol.
- Not-yet-started files: received fresh.

## Stdin / text transfers

Stdin transfers (`fsend -`) and `--text` transfers are **not resumable** in
v1. They go through the same wire protocol but the sender writes
`HasResumeSupport = false` in the FILE_INFO frame (a flag we don't have
explicitly yet — add as `FileInfo.Resumable bool`, default true for files,
false for stdin/text). Receiver doesn't write a sidecar for non-resumable
transfers.

Reason: stdin can't be re-read by the sender (stream is consumed). Forcing
the user to re-run `cat ... | fsend -` is acceptable for the rare case.

## What is NOT in v1 resume

- **No mid-transfer reconnection.** If the QUIC connection drops, the
  current transfer dies. Resume happens on the *next invocation*, not within
  the current one. Mid-flight reconnection requires session-resumption
  tokens and is a v2+ concern.
- **No resume across different codes.** A new transfer = a new code = a new
  session. The partial is keyed by *filename*, not by session, so it works
  across codes — but only when the receiver re-runs against a sender that
  produces the same content (which is the common case: the user re-runs the
  same `fsend report.pdf` on the sender side).
- **No content-defined chunking.** Chunks are fixed-size at 1 MiB. This
  means small upstream edits to a file invalidate the whole partial. CDC
  (à la rsync) is overkill for our use case.
