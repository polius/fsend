//go:build darwin

package transfer

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

const haveRenameNoReplace = true

// renameNoReplace renames src to dst unless dst exists (EEXIST), in one
// atomic call: renamex_np with RENAME_EXCL, which APFS and HFS+ support.
func renameNoReplace(src, dst string) error {
	for {
		err := unix.RenamexNp(src, dst, unix.RENAME_EXCL)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EINTR) {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
		}
	}
}

// renameNoReplaceUnsupported reports the errors a volume without
// RENAME_EXCL answers with (FAT, exFAT, some network mounts).
func renameNoReplaceUnsupported(err error) bool {
	return errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOSYS)
}
