package main

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/quicconn"
)

// After a failed TCP dial on the local network, QUIC to the same address
// is tried only when the TCP attempt gave the peer no test of the code.
func TestTCPAnswerIsFinal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no TCP route", errors.New("quicconn: tcp dial: connection refused"), false},
		{"wrong code", fmt.Errorf("%w: auth", fserrors.ErrPeerAuthFailed), true},
		{"other version", fmt.Errorf("%w: v2", fserrors.ErrIncompatibleVersion), true},
		{"data connection after a good PAKE", fmt.Errorf("%w: %w", quicconn.ErrTCPData, fserrors.ErrPeerAuthFailed), false},
		{"tag unanswered", fmt.Errorf("%w: %w", quicconn.ErrUnconfirmed, io.EOF), true},
	}
	for _, c := range cases {
		if got := tcpAnswerIsFinal(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
