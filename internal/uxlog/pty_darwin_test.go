package uxlog

import (
	"bytes"
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPty opens a pseudo-terminal pair and returns its terminal side.
func openPty() (master, tty *os.File, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	fd := int(master.Fd())
	var name [128]byte
	for _, req := range []uint{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK} {
		if err = unix.IoctlSetInt(fd, req, 0); err != nil {
			_ = master.Close()
			return nil, nil, err
		}
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), uintptr(unix.TIOCPTYGNAME), uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		_ = master.Close()
		return nil, nil, e
	}
	tty, err = os.OpenFile(string(name[:bytes.IndexByte(name[:], 0)]), os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		_ = master.Close()
	}
	return master, tty, err
}
