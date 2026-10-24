package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// emptySHA256 is the SHA-256 hex digest of a zero-byte file, reported for
// empty entries the way finalize reports non-empty ones.
var emptySHA256 = hex.EncodeToString(sha256.New().Sum(nil))

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

// progressMeter aggregates byte progress from several goroutines and reports
// it at most every 100 ms (and once at completion).
type progressMeter struct {
	mu    sync.Mutex
	done  uint64
	total uint64
	last  time.Time
	cb    func(done, total uint64)
}

func newProgressMeter(total uint64, cb func(done, total uint64)) *progressMeter {
	return &progressMeter{total: total, cb: cb}
}

func (m *progressMeter) add(n int64) {
	if m == nil || m.cb == nil || n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.done += uint64(n)
	if now := time.Now(); m.done >= m.total || now.Sub(m.last) >= 100*time.Millisecond {
		m.last = now
		m.cb(m.done, m.total)
	}
}
