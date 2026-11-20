//go:build darwin || linux

package uxlog

import (
	"testing"

	"golang.org/x/sys/unix"
)

// StopFlowControl clears IXON alone and RestoreFlowControl sets it back,
// once; a terminal that had IXON off is left alone.
func TestFlowControlToggle(t *testing.T) {
	master, tty, err := openPty()
	if err != nil {
		t.Skipf("no pty: %v", err)
	}
	defer func() { _ = tty.Close(); _ = master.Close() }()
	fd := int(tty.Fd())
	orig := foreground
	foreground = func(int) bool { return true }
	defer func() { foreground = orig }()

	get := func() (ixon, icrnl bool) {
		t.Helper()
		tio, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
		if err != nil {
			t.Fatal(err)
		}
		return tio.Iflag&unix.IXON != 0, tio.Iflag&unix.ICRNL != 0
	}
	set := func(ixon, icrnl bool) {
		t.Helper()
		tio, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
		if err != nil {
			t.Fatal(err)
		}
		tio.Iflag &^= unix.IXON | unix.ICRNL
		if ixon {
			tio.Iflag |= unix.IXON
		}
		if icrnl {
			tio.Iflag |= unix.ICRNL
		}
		if err := unix.IoctlSetTermios(fd, ioctlSetTermios, tio); err != nil {
			t.Skipf("can't set modes on this pty: %v", err)
		}
	}

	set(true, true)
	stopFlowControl(fd)
	if ixon, icrnl := get(); ixon || !icrnl {
		t.Fatalf("after stop: IXON=%v ICRNL=%v, want false true", ixon, icrnl)
	}
	set(false, false) // a mode changed meanwhile must survive the restore
	RestoreFlowControl()
	if ixon, icrnl := get(); !ixon || icrnl {
		t.Fatalf("after restore: IXON=%v ICRNL=%v, want true false", ixon, icrnl)
	}
	set(false, false)
	RestoreFlowControl() // a second restore changes nothing
	if ixon, _ := get(); ixon {
		t.Fatal("a second restore turned IXON on again")
	}

	stopFlowControl(fd) // IXON already off: nothing to undo
	RestoreFlowControl()
	if ixon, _ := get(); ixon {
		t.Fatal("restore turned on IXON that was off to begin with")
	}
}
