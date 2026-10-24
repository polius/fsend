//go:build linux || darwin || freebsd

package transfer

import (
	"errors"
	"os"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

// readUmask reads the process umask. Umask can only be read by setting it,
// so this runs once at init, before any receive creates files.
func readUmask() uint32 {
	m := syscall.Umask(0)
	syscall.Umask(m)
	return uint32(m)
}

// freeSpace reports the bytes available to this user on dir's filesystem.
func freeSpace(dir string) (uint64, bool) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, false
	}
	return uint64(st.Bavail) * uint64(st.Bsize), true
}

// fsyncData pushes a file's data to the device. On darwin this is plain
// fsync, which skips the drive-cache flush; syncBarrier then flushes the
// drive cache once for a whole batch.
func fsyncData(f *os.File) error { return unix.Fsync(int(f.Fd())) }

// syncBarrier makes every fsyncData'd file durable. darwin's F_FULLFSYNC
// (os.File.Sync) flushes the whole drive cache, so one call covers a batch.
func syncBarrier(f *os.File) error {
	if runtime.GOOS == "darwin" {
		return f.Sync()
	}
	return nil
}

func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }
