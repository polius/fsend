package transfer

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// processUmask is read once, before any receive creates files.
var processUmask = readUmask()

// fileMode is the mode a received file gets: the sender's permission bits
// filtered by the local umask, always owner-readable and -writable so a
// hostile 0000 or a world-writable 0777 can't land as-is.
func fileMode(perm uint32) os.FileMode {
	return os.FileMode(perm&^processUmask)&os.ModePerm | 0o600
}

// dirMode is fileMode for directories: always owner rwx, so a later run can
// still write into a folder that arrived read-only.
func dirMode(perm uint32) os.FileMode {
	return os.FileMode(perm&^processUmask)&os.ModePerm | 0o700
}

// fsSemantics is how the receiving filesystem compares names and stores
// times, probed once per receive.
type fsSemantics struct {
	foldCase    bool // "A" and "a" name the same file
	foldNorm    bool // NFC and NFD spellings name the same file
	coarseMtime bool // mtimes are stored to 2 s (FAT, exFAT)
}

// osDefaultSemantics is the fallback when the probe can't run.
func osDefaultSemantics() fsSemantics {
	switch runtime.GOOS {
	case "darwin":
		return fsSemantics{foldCase: true, foldNorm: true}
	case "windows":
		return fsSemantics{foldCase: true}
	}
	return fsSemantics{}
}

// probeFS creates a short-lived hidden file in dir and checks which other
// spellings reach it, so collision checks follow the real filesystem rather
// than an OS guess (case-sensitive APFS, case-insensitive ext4 dirs, …).
func probeFS(dir string) fsSemantics {
	var rnd [6]byte
	_, _ = rand.Read(rnd[:])
	tag := hex.EncodeToString(rnd[:])
	base := ".fsend-probe-Å-" + tag // Å, precomposed (NFC)
	p := filepath.Join(dir, base)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return osDefaultSemantics()
	}
	_ = f.Close()
	defer func() { _ = os.Remove(p) }()

	var sem fsSemantics
	if _, err := os.Lstat(filepath.Join(dir, ".fsend-probe-å-"+tag)); err == nil {
		sem.foldCase = true
	}
	if _, err := os.Lstat(filepath.Join(dir, ".fsend-probe-Å-"+tag)); err == nil {
		sem.foldNorm = true
	}
	// An odd second plus a fraction survives intact everywhere but FAT-family
	// filesystems, which round to an even second.
	want := time.Unix(1_600_000_001, 500_000_000)
	if os.Chtimes(p, want, want) == nil {
		if st, err := os.Stat(p); err == nil && st.ModTime().Unix() != want.Unix() {
			sem.coarseMtime = true
		}
	}
	return sem
}

var caseFolder = cases.Fold()

// nameKey is the identity of a relative path on this filesystem: two paths
// with the same key land on the same file.
func (s fsSemantics) nameKey(rel string) string {
	rel = filepath.ToSlash(rel)
	if s.foldNorm {
		rel = norm.NFC.String(rel)
	}
	if s.foldCase {
		rel = caseFolder.String(rel)
	}
	return rel
}

// sameMtime compares a local mtime with the sender's, allowing for the 2 s
// granularity of FAT-family filesystems.
func (s fsSemantics) sameMtime(local time.Time, remote int64) bool {
	d := local.Unix() - remote
	if s.coarseMtime {
		return d >= -2 && d <= 2
	}
	return d == 0
}

// errTargetAppeared: something was created at the target after it was
// classified as absent; placing the new file would replace it unasked.
var errTargetAppeared = errors.New("a file appeared at the target during the transfer")

// placer moves verified partials onto targets that must still be absent.
// One per receive, used by one goroutine (the committer).
type placer struct {
	// noExcl: the one-call exclusive rename doesn't work on this
	// filesystem (FAT, a network mount, an old kernel). Remembered so the
	// rest of the receive doesn't probe it again for every file.
	noExcl bool
	// excl overrides renameNoReplace — tests stand in for a filesystem
	// without it.
	excl func(src, dst string) error
}

// placeNew moves src to dst only if dst does not exist. Where the OS has an
// exclusive rename (renameNoReplace) that is one atomic call; a hard link
// plus an unlink does the same in two, and costs four times as much per file
// on APFS, so it is only the fallback (placeByLink).
func (pl *placer) placeNew(src, dst string) error {
	var probeErr error
	if haveRenameNoReplace && !pl.noExcl {
		excl := renameNoReplace
		if pl.excl != nil {
			excl = pl.excl
		}
		err := excl(src, dst)
		if err == nil {
			return nil
		}
		if errors.Is(err, fs.ErrExist) {
			return errTargetAppeared
		}
		probeErr = err
	}
	// Any other failure goes to the portable path, which either places the
	// file or reports the real cause (a refused name, another filesystem).
	err := placeByLink(src, dst)
	if err == nil && probeErr != nil && renameNoReplaceUnsupported(probeErr) {
		pl.noExcl = true
	}
	return err
}

// placeByLink is placeNew without an exclusive rename. A hard link fails on
// an existing name atomically; where links aren't available (FAT, exFAT,
// another filesystem) it checks and renames, which narrows the window to
// microseconds instead of the whole transfer.
func placeByLink(src, dst string) error {
	err := os.Link(src, dst)
	if err == nil {
		_ = os.Remove(src)
		return nil
	}
	if os.IsExist(err) {
		return errTargetAppeared
	}
	if _, lerr := os.Lstat(dst); lerr == nil {
		return errTargetAppeared
	}
	return moveFile(src, dst)
}

// moveFile renames src over dst, falling back to copy+sync+rename when they
// sit on different filesystems (a mount point inside the receive folder).
func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil || !isCrossDevice(err) {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".fsend-move-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if st, err := in.Stat(); err == nil {
		_ = tmp.Chmod(st.Mode().Perm())
		_ = os.Chtimes(tmp.Name(), st.ModTime(), st.ModTime())
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return err
	}
	_ = os.Remove(src)
	return nil
}

// peerSafe renders a local failure for the peer: the operation, the path
// relative to the transfer, and the error class — never the receiver's
// absolute paths.
func peerSafe(op, rel string, err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	if rel == "" {
		return fmt.Sprintf("%s: %v", op, err)
	}
	return fmt.Sprintf("%s %s: %v", op, rel, err)
}
