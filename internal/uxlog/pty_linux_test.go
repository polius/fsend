package uxlog

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openPty opens a pseudo-terminal pair and returns its terminal side.
func openPty() (master, tty *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	if err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err == nil {
		var n int
		if n, err = unix.IoctlGetInt(fd, unix.TIOCGPTN); err == nil {
			tty, err = os.OpenFile("/dev/pts/"+strconv.Itoa(n), os.O_RDWR|unix.O_NOCTTY, 0)
		}
	}
	if err != nil {
		_ = master.Close()
	}
	return master, tty, err
}
