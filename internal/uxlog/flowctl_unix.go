//go:build darwin || freebsd || openbsd || netbsd || dragonfly || linux

package uxlog

import (
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

var flowMu sync.Mutex

// flowFd is the terminal whose IXON StopFlowControl cleared, -1 for none.
var flowFd = -1

// StopFlowControl switches off output flow control (IXON) on the terminal
// stderr writes to, for the rest of the run. Every write to stderr happens
// under ttyMu; with flow control on, a Ctrl-S pauses the terminal, the next
// write blocks holding that lock, and every goroutine that prints — the
// pairing and transfer code among them — stalls behind it. Ctrl-S and
// Ctrl-Q then reach fsend as ordinary input, which it ignores.
//
// It does nothing when stderr is not a terminal, when fsend is a background
// job (changing modes from the background would stop it with SIGTTOU), or
// when IXON is already off. RestoreFlowControl undoes it.
func StopFlowControl() { stopFlowControl(int(os.Stderr.Fd())) }

func stopFlowControl(fd int) {
	if !foreground(fd) {
		return
	}
	t, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil || t.Iflag&unix.IXON == 0 {
		return
	}
	t.Iflag &^= unix.IXON
	flowMu.Lock()
	defer flowMu.Unlock()
	if unix.IoctlSetTermios(fd, ioctlSetTermios, t) == nil {
		flowFd = fd
	}
}

// RestoreFlowControl turns IXON back on if StopFlowControl cleared it. It
// changes only that bit, so other modes set meanwhile survive, and it is
// safe to call more than once or from a signal path.
func RestoreFlowControl() {
	flowMu.Lock()
	defer flowMu.Unlock()
	if flowFd < 0 {
		return
	}
	fd := flowFd
	flowFd = -1
	if !foreground(fd) {
		return
	}
	if t, err := unix.IoctlGetTermios(fd, ioctlGetTermios); err == nil {
		t.Iflag |= unix.IXON
		_ = unix.IoctlSetTermios(fd, ioctlSetTermios, t)
	}
}

// foreground reports whether fd is a terminal and this process's group is
// its foreground job. A var so tests can use a pty they don't control.
var foreground = func(fd int) bool {
	pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	return err == nil && pgrp == unix.Getpgrp()
}
