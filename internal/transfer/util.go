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
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}
