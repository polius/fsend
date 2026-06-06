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

// symlinkEscapes reports whether placing a symlink at <targetDir>/<relPath>
// pointing at <linkTarget> would resolve outside targetDir. Used by Recv
// to reject hostile peer-supplied symlinks before any subsequent file
// write can traverse through them.
//
// Two rejection paths:
//   - linkTarget is absolute → always rejected (no useful absolute symlink
//     could land safely inside a receiver's TargetDir).
//   - linkTarget is relative → resolved from the symlink's parent dir,
//     then checked against targetDir lexically. A relative symlink that
//     stays inside the tree (e.g. "../sister" within a nested dir) is
//     accepted.
func symlinkEscapes(targetDir, relPath, linkTarget string) bool {
	if filepath.IsAbs(linkTarget) {
		return true
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return true // can't verify → safest to reject
	}
	symlinkDir := filepath.Join(absTarget, filepath.Dir(relPath))
	resolved := filepath.Clean(filepath.Join(symlinkDir, linkTarget))
	return !pathIsUnder(resolved, absTarget)
}
