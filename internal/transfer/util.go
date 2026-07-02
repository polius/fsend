package transfer

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"
)

// hashPrefixInto streams the first n bytes of f through h. Used to
// hydrate the resume verifier so the final BLAKE3 root check covers
// the whole assembled file, including the prefix we kept on disk.
//
// Seeks f back to 0 before reading. The caller is responsible for
// seeking forward again afterwards.
func hashPrefixInto(h *blake3.Hasher, f *os.File, n int64) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := io.CopyN(h, f, n)
	return err
}

// fsyncDir flushes a directory's entries to stable storage so a rename
// into it survives a crash. Best-effort: directory fsync is a POSIX notion
// not supported on every platform (notably Windows), so errors are ignored.
// Durability of the file *contents* is guaranteed separately by syncing the
// file before it is renamed into place.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// pathIsUnder reports whether child is the same as parent or lives below it
// in the filesystem hierarchy. Both must be absolute and clean.
func pathIsUnder(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	// A leading ".." is a real escape only when it is the ".." element itself
	// (rel == ".." or "../…"), not a filename that merely starts with two dots
	// — e.g. "..data", which Kubernetes secret mounts create. The old
	// HasPrefix(rel, "..") test rejected those legitimate in-bounds names.
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(rel)
}
