//go:build linux

package transfer

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const haveRenameNoReplace = true

// renameNoReplace renames src to dst unless dst exists (EEXIST), in one
// atomic call: renameat2 with RENAME_NOREPLACE (Linux 3.15; ext4, xfs,
// btrfs, tmpfs and most others).
func renameNoReplace(src, dst string) error {
	for {
		err := unix.Renameat2(unix.AT_FDCWD, src, unix.AT_FDCWD, dst, unix.RENAME_NOREPLACE)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EINTR) {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
		}
	}
}

// renameNoReplaceUnsupported reports the errors a kernel or filesystem
// without RENAME_NOREPLACE answers with: ENOSYS before 3.15, EINVAL from a
// filesystem that doesn't take the flag (some FUSE and network mounts),
// ENOTSUP/EOPNOTSUPP from others.
func renameNoReplaceUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EINVAL) ||
		errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
