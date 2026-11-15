//go:build windows

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const haveRenameNoReplace = true

// renameNoReplace renames src to dst unless dst exists, in one atomic call:
// MoveFileEx without MOVEFILE_REPLACE_EXISTING (os.Rename passes that flag)
// fails with ERROR_ALREADY_EXISTS on an existing name. Without
// MOVEFILE_COPY_ALLOWED another volume is an error too, which the fallback
// handles with its copy.
func renameNoReplace(src, dst string) error {
	from, err := windows.UTF16PtrFromString(src)
	if err == nil {
		var to *uint16
		if to, err = windows.UTF16PtrFromString(dst); err == nil {
			err = windows.MoveFileEx(from, to, 0)
		}
	}
	if err != nil {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: err}
	}
	return nil
}

// renameNoReplaceUnsupported reports a volume whose driver doesn't
// implement the move (some network redirectors).
func renameNoReplaceUnsupported(err error) bool {
	return errors.Is(err, errors.ErrUnsupported) || errors.Is(err, windows.ERROR_INVALID_FUNCTION)
}
