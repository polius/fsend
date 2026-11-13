package transfer

import (
	"path/filepath"

	"github.com/polius/fsend/internal/wire"
)

// Helpers only the tests use, kept out of the shipped binary.

// classify resolves every listing entry against targetDir with the probed
// filesystem semantics. Kept as the simple entry point for tests.
func classify(entries []wire.ListingEntry, targetDir string, checksum bool) ([]entryPlan, error) {
	c := &classifier{targetDir: targetDir, checksum: checksum, sem: probeFS(targetDir), ps: newPartialStore(targetDir)}
	return c.run(entries)
}

// bytesRoot is the tree root of an in-memory buffer (tests, small inputs).
func bytesRoot(b []byte) [32]byte {
	h := newFileHasher()
	_, _ = h.Write(b)
	return h.Sum()
}

// hasPartials reports whether any partial data is kept.
func (ps *partialStore) hasPartials() bool {
	m, _ := filepath.Glob(filepath.Join(ps.dir, "*.partial"))
	return len(m) > 0
}
