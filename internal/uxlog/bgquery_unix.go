//go:build darwin || freebsd || openbsd || netbsd || dragonfly || linux

package uxlog

import (
	"bytes"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// bgQuery asks for the background colour (OSC 11) and then for the device
// attributes (DA1). Every terminal answers DA1 and answers in order, so its
// reply marks the end: no guessing whether an OSC 11 answer is still on the
// way, and nothing of ours is left in the input buffer.
const bgQuery = "\x1b]11;?\x1b\\\x1b[c"

// bgQuerySilence is how long the terminal may stay silent before fsend
// gives up (a pseudo-terminal with no emulator behind it never answers).
// Real terminals answer within milliseconds; the read ends as soon as the
// DA1 reply arrives. In tenths of a second, the unit termios VTIME uses.
const bgQuerySilence = 3

// queryBackground asks the controlling terminal for its background colour
// and classifies it. Any failure — no terminal, a background job, a
// terminal that stays silent — is themeUnknown, which renders in the
// terminal's own ANSI colours.
func queryBackground() theme {
	f, err := os.OpenFile("/dev/tty", os.O_RDWR|unix.O_NOCTTY, 0)
	if err != nil {
		return themeUnknown
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd())
	// Only the foreground job may change terminal modes; a background job
	// that tried would be stopped by SIGTTOU.
	if pgrp, err := unix.IoctlGetInt(fd, unix.TIOCGPGRP); err != nil || pgrp != unix.Getpgrp() {
		return themeUnknown
	}
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return themeUnknown
	}
	raw := *old
	raw.Lflag &^= unix.ECHO | unix.ICANON
	raw.Cc[unix.VMIN] = 0
	raw.Cc[unix.VTIME] = bgQuerySilence
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &raw); err != nil {
		return themeUnknown
	}
	defer func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }()
	if _, err := unix.Write(fd, []byte(bgQuery)); err != nil {
		return themeUnknown
	}
	var reply []byte
	buf := make([]byte, 256)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && len(reply) < 4096 {
		n, err := unix.Read(fd, buf)
		if err == unix.EINTR {
			continue
		}
		if n <= 0 {
			break // silence: the terminal has said all it will
		}
		reply = append(reply, buf[:n]...)
		if daReplied(reply) {
			break
		}
	}
	if th, ok := parseOSC11(reply); ok {
		return th
	}
	return themeUnknown
}

// daReplied reports whether reply holds a complete DA1 answer
// ("ESC [ ? … c").
func daReplied(reply []byte) bool {
	i := bytes.Index(reply, []byte("\x1b[?"))
	return i >= 0 && bytes.IndexByte(reply[i:], 'c') > 0
}
