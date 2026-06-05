package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
)

// timeAfter500ms is the shared deadline for the deadlock-regression
// tests. Used as a generous upper bound on "drainBoth/drainLoser must
// return immediately when its channel is already empty".
func timeAfter500ms() <-chan time.Time { return time.After(500 * time.Millisecond) }

func TestPickFinalSendError(t *testing.T) {
	cases := []struct {
		name      string
		lanErr    error
		serverErr error
		want      error
	}{
		{
			name:      "server-down wins over LAN error",
			lanErr:    errors.New("LAN listener: port in use"),
			serverErr: fmt.Errorf("dialing: %w", fserrors.ErrServerUnreachable),
			want:      fserrors.ErrServerUnreachable,
		},
		{
			name:      "LAN error returned when server returned non-unreachable error",
			lanErr:    errors.New("LAN listener: port in use"),
			serverErr: errors.New("some other server error"),
			want:      errors.New("LAN listener: port in use"),
		},
		{
			name:      "server-only failure",
			lanErr:    nil,
			serverErr: fserrors.ErrServerUnreachable,
			want:      fserrors.ErrServerUnreachable,
		},
		{
			name:      "LAN-only failure",
			lanErr:    errors.New("mDNS announce failed"),
			serverErr: nil,
			want:      errors.New("mDNS announce failed"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pickFinalSendError(tc.lanErr, tc.serverErr)
			// Use errors.Is for the server-unreachable case; structural
			// equality for the rest.
			if errors.Is(tc.want, fserrors.ErrServerUnreachable) {
				if !errors.Is(got, fserrors.ErrServerUnreachable) {
					t.Errorf("got %v, want errors.Is(ErrServerUnreachable)", got)
				}
				return
			}
			if (got == nil) != (tc.want == nil) || (got != nil && got.Error() != tc.want.Error()) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsServerDown(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"server unreachable wrapped", fmt.Errorf("dialing: %w", fserrors.ErrServerUnreachable), true},
		{"server unreachable plain", fserrors.ErrServerUnreachable, true},
		{"code not found", fserrors.ErrCodeNotFound, false},
		{"random error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isServerDown(tc.err); got != tc.want {
				t.Errorf("isServerDown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// drainBoth must release pair resources for both channels even when
// only one carried a real pairing. Specifically: a buffered outcome
// reporting an err must not leak resources, and a buffered outcome
// carrying a pairing must have its cleanup invoked.
func TestDrainBoth_RunsCleanupOnAnyLeftoverPairing(t *testing.T) {
	var lanCleaned, serverCleaned bool
	lanPair := &lanSenderPairing{cleanup: func() { lanCleaned = true }}
	serverPair := &internetSenderPairing{cleanup: func() { serverCleaned = true }}

	lanCh := make(chan sendPairOutcome, 1)
	serverCh := make(chan sendPairOutcome, 1)
	lanCh <- sendPairOutcome{lan: lanPair}
	serverCh <- sendPairOutcome{server: serverPair}

	drainBoth(lanCh, serverCh, false /*lanDone*/, false /*serverDone*/)

	if !lanCleaned {
		t.Error("LAN cleanup was not called")
	}
	if !serverCleaned {
		t.Error("server cleanup was not called")
	}
}

// Regression for the deadlock: if the main loop already consumed one
// channel's outcome before deciding to bail, drainBoth must not try to
// read from that empty channel — it would block forever.
func TestDrainBoth_SkipsAlreadyConsumedChannel(t *testing.T) {
	var lanCleaned bool
	lanCh := make(chan sendPairOutcome, 1)
	serverCh := make(chan sendPairOutcome, 1)
	lanCh <- sendPairOutcome{lan: &lanSenderPairing{cleanup: func() { lanCleaned = true }}}
	// serverCh intentionally empty — server's outcome was already drained.

	done := make(chan struct{})
	go func() {
		drainBoth(lanCh, serverCh, false, true /*serverDone*/)
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter500ms():
		t.Fatal("drainBoth deadlocked when serverDone=true")
	}
	if !lanCleaned {
		t.Error("LAN cleanup was not called")
	}
}

// drainBoth should also tolerate the case where one or both channels
// carry only an error (the goroutine ran but produced no pairing) —
// nothing to clean up, no panic.
func TestDrainBoth_NoLeftoverPairings(t *testing.T) {
	lanCh := make(chan sendPairOutcome, 1)
	serverCh := make(chan sendPairOutcome, 1)
	lanCh <- sendPairOutcome{err: errors.New("lan failed")}
	serverCh <- sendPairOutcome{err: errors.New("server failed")}

	drainBoth(lanCh, serverCh, false, false)
}

// drainLoser cleans up the loser path's pairing if it raced to a
// successful pair just before the coordinator cancelled the pair ctx.
func TestDrainLoser_CleansLoserPairing(t *testing.T) {
	t.Run("server lost", func(t *testing.T) {
		var serverCleaned bool
		loser := &internetSenderPairing{cleanup: func() { serverCleaned = true }}
		lanCh := make(chan sendPairOutcome, 1)
		serverCh := make(chan sendPairOutcome, 1)
		serverCh <- sendPairOutcome{server: loser}

		winner := sendPairOutcome{lan: &lanSenderPairing{cleanup: func() {}}}
		drainLoser(lanCh, serverCh, winner, true /*lanDone*/, false /*serverDone*/)
		if !serverCleaned {
			t.Error("loser (server) cleanup was not called")
		}
	})
	t.Run("lan lost", func(t *testing.T) {
		var lanCleaned bool
		loser := &lanSenderPairing{cleanup: func() { lanCleaned = true }}
		lanCh := make(chan sendPairOutcome, 1)
		serverCh := make(chan sendPairOutcome, 1)
		lanCh <- sendPairOutcome{lan: loser}

		winner := sendPairOutcome{server: &internetSenderPairing{cleanup: func() {}}}
		drainLoser(lanCh, serverCh, winner, false /*lanDone*/, true /*serverDone*/)
		if !lanCleaned {
			t.Error("loser (LAN) cleanup was not called")
		}
	})
}

// Regression: the loser's outcome may have already been consumed in
// the main coordinator loop (e.g., server reported unreachable before
// LAN paired). drainLoser must skip reading the channel in that case.
func TestDrainLoser_SkipsAlreadyConsumedChannel(t *testing.T) {
	lanCh := make(chan sendPairOutcome, 1)
	serverCh := make(chan sendPairOutcome, 1)
	// Both channels empty — winner LAN, server already drained.
	winner := sendPairOutcome{lan: &lanSenderPairing{cleanup: func() {}}}

	done := make(chan struct{})
	go func() {
		drainLoser(lanCh, serverCh, winner, true, true)
		close(done)
	}()
	select {
	case <-done:
	case <-timeAfter500ms():
		t.Fatal("drainLoser deadlocked when serverDone=true")
	}
}

// drainLoser must also tolerate the loser reporting an error before
// pair ctx was even cancelled — without trying to dereference a nil
// pairing.
func TestDrainLoser_LoserErrored(t *testing.T) {
	t.Run("server errored", func(t *testing.T) {
		lanCh := make(chan sendPairOutcome, 1)
		serverCh := make(chan sendPairOutcome, 1)
		serverCh <- sendPairOutcome{err: errors.New("cancelled")}

		winner := sendPairOutcome{lan: &lanSenderPairing{cleanup: func() {}}}
		drainLoser(lanCh, serverCh, winner, true, false) // must not panic
	})
	t.Run("lan errored", func(t *testing.T) {
		lanCh := make(chan sendPairOutcome, 1)
		serverCh := make(chan sendPairOutcome, 1)
		lanCh <- sendPairOutcome{err: errors.New("cancelled")}

		winner := sendPairOutcome{server: &internetSenderPairing{cleanup: func() {}}}
		drainLoser(lanCh, serverCh, winner, false, true)
	})
}
