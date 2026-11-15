//go:build darwin || freebsd

package transfer

import (
	"os"
	"syscall"
)

// changeTime is the inode change time of fi in nanoseconds: it moves on
// every change to the file's content or metadata and can't be set from
// user space, unlike the modification time. 0 when fi doesn't carry it.
func changeTime(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Ctimespec.Nano()
	}
	return 0
}
