// Package syserr classifies OS errors portably. Go's syscall.Errno on
// Windows maps only the os.Err* sentinels, so errors.Is(err,
// syscall.ENOSPC) and friends never match there: Windows reports
// WSA*/ERROR_* codes instead. Each helper checks both spellings.
package syserr

import (
	"errors"
	"syscall"
)

// IsNoSpace reports a full disk (ENOSPC, ERROR_DISK_FULL).
func IsNoSpace(err error) bool { return anyOf(err, syscall.ENOSPC, winNoSpace...) }

// IsConnRefused reports a refused connection.
func IsConnRefused(err error) bool { return anyOf(err, syscall.ECONNREFUSED, winConnRefused...) }

// IsNoRoute reports an unreachable or down network or host: what a
// network change or a dropped link looks like.
func IsNoRoute(err error) bool {
	return anyOf(err, syscall.ENETUNREACH, winNoRoute...) ||
		anyOf(err, syscall.EHOSTUNREACH) || anyOf(err, syscall.ENETDOWN)
}

// IsAddrNotAvail reports a local address that went away (EADDRNOTAVAIL).
func IsAddrNotAvail(err error) bool { return anyOf(err, syscall.EADDRNOTAVAIL, winAddrNotAvail...) }

// IsMsgTooLong reports a datagram larger than the read buffer. Windows
// fails the read (WSAEMSGSIZE) instead of truncating silently.
func IsMsgTooLong(err error) bool { return anyOf(err, syscall.EMSGSIZE, winMsgSize...) }

func anyOf(err error, unix syscall.Errno, win ...syscall.Errno) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, unix) {
		return true
	}
	for _, e := range win {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
