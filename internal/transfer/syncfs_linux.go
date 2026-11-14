//go:build linux

package transfer

import (
	"os"

	"golang.org/x/sys/unix"
)

// syncWholeFS flushes every dirty page and inode of the filesystem holding
// f with one syncfs(2) — durable for a whole commit batch at once.
func syncWholeFS(f *os.File) (bool, error) {
	return true, unix.Syncfs(int(f.Fd()))
}
