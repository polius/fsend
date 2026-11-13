//go:build linux

package uxlog

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)
