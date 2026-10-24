//go:build !windows

package syserr

import "syscall"

var winNoSpace, winConnRefused, winNoRoute, winAddrNotAvail, winMsgSize []syscall.Errno
