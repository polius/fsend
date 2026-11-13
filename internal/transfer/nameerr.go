package transfer

import (
	"errors"
	"runtime"
	"syscall"
)

// isNameErr reports a filesystem refusing a name itself — invalid UTF-8 on
// APFS (EILSEQ), longer than it allows (ENAMETOOLONG), characters a FAT or
// SMB volume forbids (EINVAL), Windows' ERROR_INVALID_NAME and
// ERROR_FILENAME_EXCED_RANGE. Such an entry is refused on its own; any other
// write failure still ends the transfer.
func isNameErr(err error) bool {
	if errors.Is(err, syscall.EILSEQ) || errors.Is(err, syscall.ENAMETOOLONG) || errors.Is(err, syscall.EINVAL) {
		return true
	}
	if runtime.GOOS == "windows" {
		var en syscall.Errno
		if errors.As(err, &en) && (en == 123 || en == 206) {
			return true
		}
	}
	return false
}

// nameRefusedReason is what both sides are told for such an entry.
const nameRefusedReason = "this filesystem doesn't accept the name"
