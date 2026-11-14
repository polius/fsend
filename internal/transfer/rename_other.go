//go:build !darwin && !linux && !windows

package transfer

import "errors"

// golang.org/x/sys has no exclusive rename for FreeBSD or OpenBSD, so there
// a new file is placed with a hard link (placeByLink), which their native
// filesystems support.
const haveRenameNoReplace = false

func renameNoReplace(string, string) error { return errors.ErrUnsupported }

func renameNoReplaceUnsupported(error) bool { return true }
