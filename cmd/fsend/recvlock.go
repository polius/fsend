package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/polius/fsend/internal/fserrors"
)

// errReceivingHere is E003 for a code another fsend on this device is
// already receiving. Without the lock behind it, a second `fsend <code>`
// here would find the first one's saved resume key and wait a minute and
// a half for a sender that is busy with the first.
var errReceivingHere = fmt.Errorf("%w: another fsend on this device is receiving it", fserrors.ErrCodeAlreadyClaimed)

// lockReceive takes this device's lock on receiving c and returns its
// release. The lock is an advisory file lock next to the resume key, so
// the system drops it when the process dies, however it dies. It is best
// effort like the resume key itself: with no usable config directory (or
// on a system without file locks) the receive goes ahead unlocked.
func lockReceive(c string) (release func(), err error) {
	none := func() {}
	rp, err := resumePath(c)
	if err != nil {
		return none, nil
	}
	p := rp + ".lock"
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return none, nil
	}
	// The holder removes the file when it is done, so a lock won on a
	// file that is no longer at p (opened just before that removal) locks
	// nothing: look again. Three rounds cover any real interleaving.
	for range 3 {
		f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: a path inside the config dir, named by a hash
		if err != nil {
			return none, nil
		}
		got, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return none, nil
		}
		if !got {
			_ = f.Close()
			return none, errReceivingHere
		}
		held, herr := f.Stat()
		named, nerr := os.Stat(p)
		if herr != nil || nerr != nil || !os.SameFile(held, named) {
			_ = f.Close()
			continue
		}
		return func() {
			// Remove before the close releases the lock, so nobody can
			// lock the file between the two. Windows cannot remove an open
			// file: there the order is the other way round, and a new
			// holder in between just keeps the file.
			_ = os.Remove(p)
			_ = f.Close()
			if runtime.GOOS == "windows" {
				_ = os.Remove(p)
			}
		}, nil
	}
	return none, nil
}
