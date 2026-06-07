package transfer

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/fserrors"
)

// ArchiveName is the wire-level "filename" used for directory archives.
// The receiver never writes this name to disk; it's the partial-file
// label during transfer and a placeholder in FILE_INFO.
//
// Kept fixed (rather than e.g. the source directory's basename) so the
// receiver-side detection logic is unambiguous and doesn't depend on
// peer-controlled strings.
const ArchiveName = "fsend-archive.tar"

// archivePartialName is the on-disk partial-file name during archive
// transfer. Lives in the target directory; cleaned up on success or
// abort. The leading dot keeps it hidden on Unix shells.
const archivePartialName = ".fsend-archive-recv"

// excludeMatcher matches a path (relative to the archive root) against a
// list of glob patterns. Patterns use filepath.Match semantics (`*`,
// `?`, character classes — no `**`). A pattern matches if it equals
// any path component or matches the full relative path.
type excludeMatcher struct {
	patterns []string
}

func newExcludeMatcher(patterns []string) excludeMatcher {
	// Trim + drop empties so `--exclude ""` is a no-op rather than a
	// silent match-everything bomb.
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return excludeMatcher{patterns: out}
}

// match reports whether the given relative path should be excluded.
// Matches if any pattern matches:
//   - the full relative path (e.g. `target/debug` matches `target/debug`)
//   - any individual path component (e.g. `target/debug/x` matches `target` if
//     the user wrote `--exclude target`)
//
// This gives the intuitive behavior most users expect from
// `--exclude node_modules` without forcing them to write
// `node_modules/**`.
func (m excludeMatcher) match(rel string) bool {
	if len(m.patterns) == 0 {
		return false
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	for _, pat := range m.patterns {
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
		for _, part := range parts {
			if ok, _ := filepath.Match(pat, part); ok {
				return true
			}
		}
	}
	return false
}

// ArchiveResult bundles what the caller needs to feed the archive into
// the existing single-file send path.
type ArchiveResult struct {
	Path       string   // absolute path of the temp tar
	Size       int64    // size in bytes
	Blake3Root [32]byte // BLAKE3 of the tar bytes
	NumEntries int      // every tar entry (files, dirs, symlinks)
	NumFiles   int      // user-facing count: regular files + symlinks only
}

// BuildArchive packages the given source paths into a single deterministic
// tar file on disk. The tar is uncompressed — per-chunk zstd in the wire
// layer handles compression adaptively, which is cheaper for already-
// compressed payloads (mp4, jpg, zstd-pre-compressed blobs) than running
// gzip/zstd here and then having writeChunk decide it didn't help.
//
// Determinism matters because imohash on the temp file is used for
// resume: an identical input directory must produce an identical tar so
// the receiver's partial fingerprint still matches across retries. We
// sort directory entries by relative path and zero out per-file device
// numbers + uname/gname to keep the output stable across platforms.
//
// On any error the temp file is deleted before returning.
func BuildArchive(paths []string, excludes []string) (*ArchiveResult, error) {
	if len(paths) == 0 {
		return nil, errors.New("archive: no paths provided")
	}
	matcher := newExcludeMatcher(excludes)

	tmp, err := os.CreateTemp("", "fsend-archive-*.tar")
	if err != nil {
		return nil, fmt.Errorf("archive: create temp: %w", err)
	}
	tmpPath := tmp.Name()

	// Hash the tar bytes as we write so we don't need a second pass.
	hasher := blake3.New()
	mw := io.MultiWriter(tmp, hasher)
	tw := tar.NewWriter(mw)

	res := &ArchiveResult{Path: tmpPath}
	failClose := func(err error) (*ArchiveResult, error) {
		_ = tw.Close()
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return nil, err
	}

	for _, p := range paths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return failClose(fmt.Errorf("archive: %s: %w", p, err))
		}
		root := filepath.Base(abs)
		st, err := os.Lstat(abs)
		if err != nil {
			return failClose(fmt.Errorf("archive: %s: %w", p, err))
		}
		if err := addToTar(tw, abs, root, st, matcher, &res.NumEntries, &res.NumFiles); err != nil {
			return failClose(err)
		}
	}

	if err := tw.Close(); err != nil {
		return failClose(fmt.Errorf("archive: close tar: %w", err))
	}
	if err := tmp.Sync(); err != nil {
		return failClose(fmt.Errorf("archive: sync: %w", err))
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return failClose(fmt.Errorf("archive: seek: %w", err))
	}
	stat, err := tmp.Stat()
	if err != nil {
		return failClose(fmt.Errorf("archive: stat: %w", err))
	}
	if err := tmp.Close(); err != nil {
		return failClose(fmt.Errorf("archive: close temp: %w", err))
	}

	res.Size = stat.Size()
	copy(res.Blake3Root[:], hasher.Sum(nil))
	return res, nil
}

// addToTar writes one filesystem entry and (if it's a directory) recurses
// into it. relTop is the path-as-seen-inside-the-archive: the user sent
// `~/projects/foo` so entries are written as `foo/...`, preserving the
// natural extracted layout.
//
// entries counts every tar header (files, dirs, symlinks). files counts
// only data-carrying entries (regular files + symlinks) — that's the
// count the user thinks of when they say "I'm sending a folder with X
// files", and the one we surface in the artifact block.
func addToTar(tw *tar.Writer, abs string, relTop string, st os.FileInfo, m excludeMatcher, entries *int, files *int) error {
	if m.match(relTop) {
		return nil
	}

	mode := st.Mode()

	hdr := &tar.Header{
		Name:    filepath.ToSlash(relTop),
		Mode:    int64(mode.Perm()),
		ModTime: st.ModTime().Truncate(time.Second), // tar's resolution is seconds
		// Zeroed fields below keep the tar deterministic across hosts.
		Uid: 0, Gid: 0, Uname: "", Gname: "",
		Format: tar.FormatPAX,
	}

	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return fmt.Errorf("archive: readlink %s: %w", abs, err)
		}
		hdr.Typeflag = tar.TypeSymlink
		hdr.Linkname = target
		hdr.Size = 0
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("archive: write symlink header %s: %w", relTop, err)
		}
		*entries++
		*files++
		return nil

	case st.IsDir():
		hdr.Typeflag = tar.TypeDir
		hdr.Name = filepath.ToSlash(relTop) + "/"
		hdr.Size = 0
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("archive: write dir header %s: %w", relTop, err)
		}
		*entries++
		children, err := os.ReadDir(abs)
		if err != nil {
			return fmt.Errorf("archive: readdir %s: %w", abs, err)
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, e := range children {
			childAbs := filepath.Join(abs, e.Name())
			childRel := filepath.ToSlash(filepath.Join(relTop, e.Name()))
			cst, err := os.Lstat(childAbs)
			if err != nil {
				return fmt.Errorf("archive: lstat %s: %w", childAbs, err)
			}
			if err := addToTar(tw, childAbs, childRel, cst, m, entries, files); err != nil {
				return err
			}
		}
		return nil

	default:
		hdr.Typeflag = tar.TypeReg
		hdr.Size = st.Size()
		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("archive: write file header %s: %w", relTop, err)
		}
		f, err := os.Open(abs)
		if err != nil {
			return fmt.Errorf("archive: open %s: %w", abs, err)
		}
		_, copyErr := io.Copy(tw, f)
		_ = f.Close()
		if copyErr != nil {
			return fmt.Errorf("archive: copy %s: %w", abs, copyErr)
		}
		*entries++
		*files++
		return nil
	}
}

// ExtractArchive unpacks a tar produced by BuildArchive into targetDir.
//
// All entries are written under targetDir and verified to stay under it
// (defense-in-depth against zip-slip-style paths arriving from a peer
// who may be running a modified client). Symlinks are written as-is;
// the receiver's filesystem rules govern whether they resolve to
// anything sensible.
//
// When overwrite is false, the archive is pre-scanned: if any regular-file
// entry would replace something already on disk, we return
// fserrors.ErrTargetExists and write nothing. Doing this in a separate
// pass keeps a conflict from leaving the target partially extracted.
func ExtractArchive(tarPath, targetDir string, overwrite bool) error {
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return fmt.Errorf("extract: abs targetDir: %w", err)
	}

	if !overwrite {
		if err := preflightExtract(tarPath, absTarget); err != nil {
			return err
		}
	}

	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("extract: open %s: %w", tarPath, err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("extract: read header: %w", err)
		}

		clean, err := safeJoin(absTarget, hdr.Name)
		if err != nil {
			return err
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(clean, os.FileMode(hdr.Mode)&os.ModePerm); err != nil {
				return fmt.Errorf("extract: mkdir %s: %w", clean, err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
				return fmt.Errorf("extract: mkdir parent %s: %w", clean, err)
			}
			out, err := os.OpenFile(clean, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode)&os.ModePerm)
			if err != nil {
				return fmt.Errorf("extract: create %s: %w", clean, err)
			}
			if _, err := io.Copy(out, tr); err != nil {
				_ = out.Close()
				return fmt.Errorf("extract: write %s: %w", clean, err)
			}
			if err := out.Close(); err != nil {
				return fmt.Errorf("extract: close %s: %w", clean, err)
			}
			if !hdr.ModTime.IsZero() {
				_ = os.Chtimes(clean, hdr.ModTime, hdr.ModTime)
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(clean), 0o755); err != nil {
				return fmt.Errorf("extract: mkdir parent %s: %w", clean, err)
			}
			// Reject symlink targets that resolve outside the target
			// dir. This is over-strict for absolute symlinks the user
			// might legitimately want, but the safe default for an
			// archive received from a remote peer.
			if _, err := safeJoin(absTarget, filepath.Join(filepath.Dir(hdr.Name), hdr.Linkname)); err != nil {
				return err
			}
			_ = os.Remove(clean) // symlink fails if target exists
			if err := os.Symlink(hdr.Linkname, clean); err != nil {
				// Don't fail the whole extract for one un-creatable
				// symlink — Windows non-admin commonly hits this.
				continue
			}
		default:
			// Skip exotic types (block/char devices, fifos). Not
			// something fsend cares about transporting.
		}
	}
}

// preflightExtract walks the tar headers without consuming entry data and
// returns ErrTargetExists on the first regular-file entry whose
// destination already exists. Symlinks and directories don't trip the
// check — MkdirAll is idempotent, and clobbering a symlink at extract
// time is what os.Remove + os.Symlink already does.
//
// Running this as a separate pass avoids the half-extracted state that
// would result from failing mid-write.
func preflightExtract(tarPath, absTarget string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return fmt.Errorf("extract: open %s: %w", tarPath, err)
	}
	defer func() { _ = f.Close() }()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("extract: scan header: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		clean, err := safeJoin(absTarget, hdr.Name)
		if err != nil {
			return err
		}
		if st, err := os.Lstat(clean); err == nil && !st.IsDir() {
			return fmt.Errorf("%w: %s", fserrors.ErrTargetExists, clean)
		}
	}
}

// safeJoin joins base with rel and rejects anything that looks like a
// path traversal or absolute path. Used for every tar entry on
// extract; without it a malicious peer could write `../etc/passwd`.
//
// We reject loudly rather than silently normalize so the user notices
// when an archive contains hostile entries — silent stripping would
// turn a security signal into a quiet bug report later.
func safeJoin(base, rel string) (string, error) {
	slash := filepath.ToSlash(rel)
	if slash == "" {
		return "", fmt.Errorf("extract: empty entry name")
	}
	if strings.HasPrefix(slash, "/") {
		return "", fmt.Errorf("extract: refused absolute path %q", rel)
	}
	for _, part := range strings.Split(slash, "/") {
		if part == ".." {
			return "", fmt.Errorf("extract: refused path traversal %q", rel)
		}
	}
	joined := filepath.Join(base, filepath.FromSlash(slash))
	absJoined, err := filepath.Abs(joined)
	if err != nil {
		return "", fmt.Errorf("extract: abs %s: %w", joined, err)
	}
	// Defense-in-depth: even if the strict checks above missed a case
	// (e.g. symlinks resolved via t.TempDir's /var → /private/var on
	// macOS), the final result must be under base after resolution.
	absBase, err := filepath.Abs(base)
	if err != nil {
		return "", fmt.Errorf("extract: abs base %s: %w", base, err)
	}
	if !pathIsUnder(absJoined, absBase) {
		return "", fmt.Errorf("extract: refused to write outside target: %q", rel)
	}
	return absJoined, nil
}
