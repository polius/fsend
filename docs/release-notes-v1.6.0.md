# v1.6.0 — Sync-style transfers

Sending the same files again now only transfers what actually changed — fsend
skips files the other side already has, instead of sending everything every
time.

## Skip files the receiver already has

When you send files or a folder, fsend checks what's already on the other side
and **skips anything that hasn't changed** — no transfer, no prompt. So
re-running `fsend ./project` only sends the new and changed files.

- New files are sent; unchanged files are skipped.
- A file that exists but is **different** is never replaced unless you add
  `--overwrite`, so you can't accidentally clobber someone's local edits.
- fsend never deletes anything on the receiver.

Folders are also quicker to send: fsend no longer bundles them up first (so
there's no wait and no temporary disk space used), and if a transfer is
interrupted, running it again picks up where it left off.

## New options

| Option | What it does |
|---|---|
| `--dry-run` | Show what *would* be sent (new / unchanged / different) without transferring anything. |
| `--checksum` | Compare files by their actual contents instead of their size and date. Slower, but spots changes even when the dates happen to match. (Same idea as rsync's `-c`.) |

## Things that work differently now

- **`fsend .` now sends the contents of the current folder.**
- **`--overwrite` only matters when the receiver already has a file with the
  same name but different content** — it lets fsend replace it. (Identical
  files are skipped whether or not you use the flag.)

## Important: update both sides

This version isn't compatible with older versions of fsend, so **the sender and
the receiver both need to be on v1.6.0 or newer**. Update with `fsend --update`.
