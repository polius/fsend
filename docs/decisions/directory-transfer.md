# Decision: Directory transfer semantics

**Status:** Locked
**Date:** 2026-06-03
**Scope:** How fsend handles `fsend ./somedir/`, multi-path invocations,
filesystem attributes, and the receive-side merge/overwrite question.

---

## User-visible behavior

### Sending a single directory

```
$ fsend ./myproject

  Sending myproject/  (142 files, 18.4 MB)

  ──────────────────────────────────────────────

      abc-defgh-jkm

  On the other machine, run:
      fsend abc-defgh-jkm
```

### Receiver

```
  Receiving from Pol's MacBook

  ──────────────────────────────────────────────

      myproject/  (142 files, 18.4 MB)

  ──────────────────────────────────────────────

  Save to current directory? [Y/n]: _
```

Then progress shows both per-file and overall (via `vbauerster/mpb`'s
multi-bar):

```
  ✓ Direct connection established  (no relay)

  Overall:  ▕████████████░░░░░░░░░░░░░░░░░░░░▏  35%   5.3/18.4 MB
  myproject/src/main.go     ✓ done
  myproject/assets/hero.png ▕████████░░░░░░░░░░░░░░░░▏  62%   1.2 MB/s
```

### Multi-path

`fsend file1.txt file2.txt dir1/` is treated as a virtual transfer with three
roots. The receiver gets prompted once and saves all three into the target
directory (default: CWD, or `--out`).

## Filesystem model

A "transfer" is an ordered list of `FileInfo` entries (see
`wire-protocol.md`). Each entry has:

- `RelativePath`: forward-slash-delimited, no leading slash, no `..`
  segments, no drive letters (Windows-friendly normalization happens at the
  sender)
- `Size`: bytes of file data (0 for directories, 0 for symlinks)
- `Mode`: unix mode bits (`0644`, `0755`, etc.)
- `ModTime`: unix nanoseconds
- `IsDir`: true → entry is just a directory marker (no data)
- `IsSymlink`: true → `SymlinkTarget` is the link target string
- `Blake3Root`: BLAKE3 hash of full file content (used for resume validation
  and end-of-transfer verification)

### Sender-side walk

For a single directory argument `./myproject`, the sender:

1. Resolves `./myproject` to its absolute path; the directory name
   (`myproject`) becomes the transfer-root prefix.
2. Walks the tree with `filepath.WalkDir`:
   - Sorted output (lexicographic) — deterministic for testing and resume.
   - Includes directories themselves (so empty dirs are preserved).
   - Skips entries the OS rejects (permission denied, broken symlink target
     gone, etc.) and warns to stderr.
3. For each entry, builds a `FileInfo` with:
   - `RelativePath = "myproject/sub/file.txt"`  (always includes the root
     directory name)
   - Mode bits, modtime, and (for files) computes BLAKE3 root by streaming
     the file once before the transfer starts. This is the resume-validation
     hash. Costs one extra pass; on typical hardware BLAKE3 streams faster
     than disk reads, so it's cheap.
4. For multi-path (`fsend a b c`), each top-level arg becomes its own
   transfer root: paths are `a`, `a/...`, `b`, `b/...`, `c`, `c/...`.

### Receiver-side write

1. Compute target directory: `--out <dir>` if given, else CWD.
2. For each `FileInfo` (in the order received):
   - Compute target path: `targetDir + "/" + RelativePath`.
   - **Path safety check**: reject if the resolved path escapes `targetDir`
     (e.g., embedded `..`). Abort with `ErrorFrame{PROTOCOL_ERROR}`.
   - If `IsDir`: `os.MkdirAll(path, mode)`. Set modtime after.
   - If `IsSymlink`: `os.Symlink(SymlinkTarget, path)`. Skip if symlinks
     not supported on the OS (Windows without admin — log warning, continue).
   - Else (regular file):
     - Check resume sidecar (see `resume.md`).
     - Open / create the target file, write chunks, apply mode + modtime
       at end-of-file.

### Path normalization

- Sender always emits forward slashes in `RelativePath` regardless of OS.
- Receiver converts to OS-native separators when constructing target paths.
- Drive letters on Windows: stripped at the sender. Sending `C:\proj` becomes
  RelativePath `proj/...`.

## Symlinks

| Scenario | Behavior |
|---|---|
| Sender follows symlinks within the transfer root | **No.** Symlinks are transferred as symlinks (their target string), not as a copy of the target. |
| Symlink target is outside the transfer root | Transferred as-is — the receiver gets a dangling symlink, which they may resolve manually. |
| Symlink target is inside the transfer root | Receiver creates the symlink; if the target file is also in the transfer (it usually will be), it'll work after both are written. |
| Receiver OS doesn't support symlinks (Windows non-admin) | Log warning, skip the symlink. |
| Sender's OS doesn't support symlinks | `IsSymlink` is always false; nothing to do. |

## File modes

- Sender records the file's unix mode bits.
- Receiver applies them where the OS supports them (Unix).
- On Windows, mode is mostly ignored (Windows file permissions are a
  different model). The executable bit (`0111`) doesn't translate;
  receivers on Windows can manually `chmod` if needed via WSL.

## Empty directories

Preserved. An empty directory in the source tree produces a `FileInfo` with
`IsDir=true` and 0 chunks. Receiver `MkdirAll`s it.

## Overwrite semantics

Default behavior (no `--overwrite`):

| Target state | Single-file send | Directory send |
|---|---|---|
| Doesn't exist | Create and write | Create and write |
| Exists, same kind (file vs dir) | **Prompt** — "Overwrite? [y/N]" per-file in v1 | Per-file prompt for each conflict (v2: batched prompt) |
| Exists, different kind (file vs dir) | Error: "exists as <other kind>, refusing to overwrite" | Same |

With `--overwrite`:
- Existing files are overwritten in place (using the resume sidecar for
  efficiency — if the existing file matches the expected hash already,
  it's recognized as "already complete" via the FILE_ACCEPT `SKIP` action).
- Existing directories are merged (files inside are subject to the same
  per-file rules).
- A file→directory or directory→file collision still errors — `--overwrite`
  doesn't authorize destructive type changes.

`--yes` skips the receive *prompt* but does NOT imply `--overwrite`. They
compose: `--yes --overwrite` is the "full automation" combination.

## Order of operations (receiver-side, single directory)

1. Receive all `FILE_INFO` frames upfront (the sender sends them in a batch
   before any chunks). This lets the receiver:
   - Show accurate total counts and bytes in the prompt
   - Pre-create the directory structure
   - Decide overwrite/skip for the whole batch at once if `--overwrite`
2. After user confirms, sender starts sending chunks.
3. Receiver writes files in the order chunks arrive (which matches the
   `FileInfo` order).
4. Directories and symlinks are emitted before any files inside them.

**Frame-volume note:** for very large directories (say, 100k files), the
batch of `FILE_INFO` frames is non-trivial. Each FileInfo is ~150 bytes
serialized; 100k files = ~15 MB of metadata up front. That's fine over QUIC
but worth budgeting for; the receiver should stream-parse the FILE_INFO
batch and start prompting before the last one arrives. v1 implementation
can simply read them all into memory first.

## Stdin transfers (`fsend -`)

- Sender doesn't know the size. `FileInfo.Size = 0` with a `Streaming = true`
  flag in the FILE_INFO.
- Filename: `fsend-stdin-<random>` (matches croc's pattern). User can rename
  on the receive side with `--out` or after the fact.
- Progress bar shows "received X bytes" with no percentage.
- Not resumable (see `resume.md`).

## Text transfers (`fsend --text "..."`)

- Single file, name = `fsend-text-<random>.txt`, content = the string,
  mode 0644.
- Same handling as a small single-file send otherwise.

## What is NOT in v1

- **No `.gitignore`-style exclusions.** All files in the walked tree are
  sent. (Croc has `--exclude`; we'll add when needed, not preemptively.)
- **No cross-platform attribute mapping** beyond mode bits. macOS extended
  attributes, Linux xattrs, NTFS streams: not preserved.
- **No partial-tree resume that survives sender re-walks.** If the sender
  adds a file between transfer attempts, the receiver's existing partials
  for files that *did* match will still resume, but the new file is treated
  as fresh.
- **No deduplication.** Two files with identical content are sent twice.
- **No compression of the metadata batch.** Even 15 MB of FILE_INFO frames
  is fine over LAN; over the relay path it's bounded by per-session caps.
