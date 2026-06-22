# 1.6.0

Sending the same files again now only transfers what actually changed — fsend
skips whatever the other side already has, instead of re-sending everything.

## How it works

fsend checks what's already on the other side and sends only what's new or
changed, **skipping anything that's already there unchanged**.

- A file that exists but is **different** is never replaced unless you add
  `--overwrite`, so you can't accidentally clobber someone's local edits.
- fsend never deletes anything on the receiver.

For example, say you send a folder of 500 files:

```
fsend ./project
```

The first run transfers all 500. If you add a couple of files and run the same
command again, fsend sends just those new files and skips the other 500.

## New options

| Option | What it does |
|---|---|
| `--dry-run` | Show what *would* be sent (new / unchanged / different) without transferring anything. |
| `--checksum` | Use this when timestamps can't be trusted (files were copied, restored from backup, or regenerated). It compares actual file contents instead of size and date — slower, but accurate. (Same idea as rsync's `-c`.) |

## Compatibility

> [!IMPORTANT]
> This version isn't compatible with older versions of fsend, so **the sender
> and the receiver both need to be on v1.6.0 or newer**. Update with
> `fsend --update`.
