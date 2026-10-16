// Package atomicfile writes files so a reader never observes a partial or
// truncated document. Data goes to a sibling tempfile, is fsynced, then
// renamed into place, and the parent directory is fsynced best-effort so
// the rename itself survives a crash. This is the same pattern
// internal/config.Save uses for the config file, factored out for the
// update cache and received-file manifests.
package atomicfile

import (
	"os"
	"path/filepath"
)

// Write writes data to path with the given permissions, atomically. The
// tempfile is created in the destination directory so the final rename is
// same-filesystem and therefore atomic.
func Write(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	// No-op after a successful rename; removes the tempfile on every error.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	SyncDir(dir)
	return nil
}

// SyncDir flushes a directory's entries to stable storage so a rename into
// it survives a crash. Best-effort: directory fsync is a POSIX notion not
// supported on every platform (notably Windows), so errors are ignored.
// Durability of the file *contents* is guaranteed separately by syncing the
// file before it is renamed into place.
func SyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
