//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !windows

package main

import (
	"errors"
	"os"
)

// tryLockFile has no file lock to take on this system: the receive goes
// ahead unlocked (see lockReceive).
func tryLockFile(*os.File) (bool, error) {
	return false, errors.New("no file locks on this system")
}
