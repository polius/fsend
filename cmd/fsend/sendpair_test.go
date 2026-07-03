package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/signaling"
)

// shrinkWaitRetryStep makes waitForReceiver's backoff test-fast.
func shrinkWaitRetryStep(t *testing.T) {
	t.Helper()
	orig := waitRetryStep
	waitRetryStep = 2 * time.Millisecond
	t.Cleanup(func() { waitRetryStep = orig })
}

// A transient Wait failure (dropped poll, wifi roam, sleep/wake) must be
// retried, not treated as fatal — the old behavior deleted the session
// on the first blip, so the shared code died while the sender kept
// "waiting" on a path nobody could reach.
func TestWaitForReceiver_RetriesTransientFailures(t *testing.T) {
	shrinkWaitRetryStep(t)
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&calls, 1) {
		case 1, 2:
			w.WriteHeader(http.StatusBadGateway) // transient server hiccup
		case 3:
			w.WriteHeader(http.StatusNoContent) // normal long-poll tick
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`)) // receiver paired
		}
	}))
	defer ts.Close()

	resp, err := waitForReceiver(context.Background(), signaling.New(ts.URL, "test"), "abc-defg-jkm")
	if err != nil || resp == nil {
		t.Fatalf("expected pairing to survive transient failures, got resp=%v err=%v", resp, err)
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Errorf("expected 4 Wait calls (2 fail, 1 tick, 1 paired), got %d", got)
	}
}

// Persistent failure must still give up (and let the caller delete the
// session) rather than retry forever against a dead server.
func TestWaitForReceiver_GivesUpAfterConsecutiveFailures(t *testing.T) {
	shrinkWaitRetryStep(t)
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	_, err := waitForReceiver(context.Background(), signaling.New(ts.URL, "test"), "abc-defg-jkm")
	if err == nil {
		t.Fatal("expected an error from a persistently failing server")
	}
	if got := atomic.LoadInt32(&calls); got != waitMaxConsecFails {
		t.Errorf("expected exactly %d Wait calls, got %d", waitMaxConsecFails, got)
	}
}

// A reaped session (server-side TTL) is not transient: surface the
// dedicated expired error immediately.
func TestWaitForReceiver_SessionReapedSurfacesExpired(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	_, err := waitForReceiver(context.Background(), signaling.New(ts.URL, "test"), "abc-defg-jkm")
	if !errors.Is(err, fserrors.ErrSessionExpired) {
		t.Errorf("expected ErrSessionExpired for a reaped session, got %v", err)
	}
}

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

// isReceiverClose must fire only on a remote application close — the
// receiver deliberately hanging up — and not on local closes or idle
// timeouts, which keep the re-accept grace for a possible re-dial.
func TestIsReceiverClose(t *testing.T) {
	remote := &quic.ApplicationError{Remote: true}
	local := &quic.ApplicationError{Remote: false}

	if !isReceiverClose(remote) {
		t.Error("remote application close should classify as receiver close")
	}
	if !isReceiverClose(fmt.Errorf("send: chunk: %w", remote)) {
		t.Error("wrapped remote close should classify as receiver close")
	}
	if isReceiverClose(local) {
		t.Error("local close must not classify as receiver close")
	}
	if isReceiverClose(&quic.IdleTimeoutError{}) {
		t.Error("idle timeout must not classify as receiver close")
	}
	if isReceiverClose(nil) {
		t.Error("nil must not classify as receiver close")
	}

	// The receiver's teardown cancels its data-stream read (STOP_SENDING)
	// just before the connection close; an in-flight chunk write can see
	// the stream cancel first. Same deliberate close, same verdict —
	// without this it escaped to the E099 catchall.
	remoteStream := &quic.StreamError{StreamID: 3, ErrorCode: 0, Remote: true}
	if !isReceiverClose(fmt.Errorf("wire: writing chunk payload: %w", remoteStream)) {
		t.Error("remote stream cancel should classify as receiver close")
	}
	if isReceiverClose(&quic.StreamError{StreamID: 3, ErrorCode: 0, Remote: false}) {
		t.Error("local stream cancel must not classify as receiver close")
	}
}
