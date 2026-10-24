package syserr

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestClassifiesWrappedErrnos(t *testing.T) {
	wrap := func(e syscall.Errno) error { return fmt.Errorf("op: %w", &os.SyscallError{Syscall: "x", Err: e}) }
	cases := []struct {
		name string
		fn   func(error) bool
		err  error
	}{
		{"nospace", IsNoSpace, wrap(syscall.ENOSPC)},
		{"refused", IsConnRefused, wrap(syscall.ECONNREFUSED)},
		{"netunreach", IsNoRoute, wrap(syscall.ENETUNREACH)},
		{"hostunreach", IsNoRoute, wrap(syscall.EHOSTUNREACH)},
		{"addrnotavail", IsAddrNotAvail, wrap(syscall.EADDRNOTAVAIL)},
		{"msgsize", IsMsgTooLong, wrap(syscall.EMSGSIZE)},
	}
	for _, c := range cases {
		if !c.fn(c.err) {
			t.Errorf("%s: not classified", c.name)
		}
	}
	for _, e := range append(append(winNoSpace, winNoRoute...), winMsgSize...) {
		if !IsNoSpace(wrap(e)) && !IsNoRoute(wrap(e)) && !IsMsgTooLong(wrap(e)) {
			t.Errorf("windows errno %d not classified", e)
		}
	}
	if IsNoSpace(nil) || IsConnRefused(wrap(syscall.EPERM)) {
		t.Error("false positive")
	}
}
