//go:build windows

package transfer

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func readUmask() uint32 { return 0 }

func freeSpace(dir string) (uint64, bool) {
	p, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, false
	}
	var avail uint64
	if err := windows.GetDiskFreeSpaceEx(p, &avail, nil, nil); err != nil {
		return 0, false
	}
	return avail, true
}

func fsyncData(f *os.File) error { return f.Sync() }

func syncBarrier(*os.File) error { return nil }

func isCrossDevice(err error) bool { return errors.Is(err, windows.ERROR_NOT_SAME_DEVICE) }
