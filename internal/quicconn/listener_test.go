package quicconn

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
)

// A peer that completes QUIC but never runs the PAKE must not block the
// real receiver queued behind it (the serial accept loop did).
func TestListener_StalledPeerDoesNotBlockAccept(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, err := ListenAddr("127.0.0.1:0", secret)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.ln.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The impostor dials and then sits there, never accepting the control stream.
	stalled, err := quic.DialAddr(ctx, addr, ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.CloseWithError(0, "")

	recvDone := make(chan error, 1)
	go func() {
		res, err := Dial(ctx, addr, secret)
		if err == nil {
			res.Close()
		}
		recvDone <- err
	}()

	start := time.Now()
	res, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	defer res.Close()
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Accept took %v behind a stalled peer", d)
	}
	if err := <-recvDone; err != nil {
		t.Errorf("receiver: %v", err)
	}
}

// A failed PAKE closes the connection on both ends instead of leaving it
// alive on keepalives, and Accept reports it as an auth failure.
func TestListener_FailedHandshakeClosesConn(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := quic.DialAddr(ctx, ln.ln.Addr().String(), ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReceiverHandshake(ctx, c, "xyz-pqrs-tuv"); !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("receiver handshake: got %v, want ErrPeerAuthFailed", err)
	}
	select {
	case <-c.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("receiver connection still open after failed handshake")
	}
	if _, err := ln.Accept(ctx); !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("Accept: got %v, want ErrPeerAuthFailed", err)
	}
}

// SetSecret switches the PAKE secret for later handshakes.
func TestListener_SetSecret(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ln.SetSecret("resume-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		if res, err := Dial(ctx, ln.ln.Addr().String(), "resume-secret"); err == nil {
			res.Close()
		}
	}()
	res, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept with the new secret: %v", err)
	}
	res.Close()
}

// Cancel closes with CloseCancelled, which the peer can tell apart from a
// plain close or a network drop.
func TestAcceptResult_CancelReachesPeer(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, err := ListenAddr("127.0.0.1:0", secret)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	recv := make(chan *AcceptResult, 1)
	go func() {
		res, err := Dial(ctx, ln.ln.Addr().String(), secret)
		if err != nil {
			t.Errorf("Dial: %v", err)
		}
		recv <- res
	}()
	snd, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r := <-recv
	if r == nil {
		t.FailNow()
	}
	defer r.Close()
	if r.PeerCancelled(10 * time.Millisecond) {
		t.Fatal("PeerCancelled before any close")
	}
	snd.Cancel()
	if !r.PeerCancelled(2 * time.Second) {
		t.Fatalf("peer cancel not seen; cause: %v", context.Cause(r.Conn.Context()))
	}
}

// A peer the filter refuses is closed before any PAKE and is not reported
// as a failed handshake; an admitted peer pairs as usual.
func TestListener_PeerFilter(t *testing.T) {
	const secret = "abc-defg-hjk"
	var admit atomic.Bool
	ln, err := ListenAddr("127.0.0.1:0", secret, WithPeerFilter(func(net.Addr) bool { return admit.Load() }))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.ln.Addr().String()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := Dial(ctx, addr, secret); err == nil {
		t.Fatal("a refused peer completed the handshake")
	}
	select {
	case err := <-ln.fails:
		t.Fatalf("a refused peer was reported as a failed handshake: %v", err)
	default:
	}

	admit.Store(true)
	go func() {
		if res, err := Dial(ctx, addr, secret); err == nil {
			res.Close()
		}
	}()
	res, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("admitted peer: %v", err)
	}
	res.Close()
}

// After a failed PAKE the listener waits before starting the next
// handshake, longer after each failure, and a success resets the wait.
func TestListener_SlowsDownAfterFailures(t *testing.T) {
	for n, want := range map[int]time.Duration{0: 0, 1: 250 * time.Millisecond, 2: 500 * time.Millisecond, 4: 2 * time.Second, 5: 4 * time.Second, 50: 4 * time.Second} {
		if got := failPenalty(n); got != want {
			t.Errorf("failPenalty(%d) = %v, want %v", n, got, want)
		}
	}

	const secret = "abc-defg-hjk"
	ln, err := ListenAddr("127.0.0.1:0", secret)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	addr := ln.ln.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	for i := 1; i <= 2; i++ {
		if _, err := Dial(ctx, addr, "xyz-pqrs-tuv"); !errors.Is(err, fserrors.ErrPeerAuthFailed) {
			t.Fatalf("impostor %d: got %v, want ErrPeerAuthFailed", i, err)
		}
		if _, err := ln.Accept(ctx); !errors.Is(err, fserrors.ErrPeerAuthFailed) {
			t.Fatalf("Accept after impostor %d: got %v", i, err)
		}
	}
	// Two failures in a row: the next handshake waits failPenalty(2).
	start := time.Now()
	go func() {
		if res, err := Dial(ctx, addr, secret); err == nil {
			res.Close()
		}
	}()
	res, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("receiver after failures: %v", err)
	}
	res.Close()
	if d := time.Since(start); d < failPenalty(2)-50*time.Millisecond {
		t.Errorf("the handshake after two failures started after %v, want at least %v", d, failPenalty(2))
	}
	ln.mu.Lock()
	run := ln.failsRun
	ln.mu.Unlock()
	if run != 0 {
		t.Errorf("a successful handshake left %d failures on the count", run)
	}
}

// A peer that gives up before the PAKE shares have crossed (cancelled,
// timed out) did not fail the PAKE, even though it closes with the
// handshake-failed code.
func TestListener_EarlyCloseIsNotAuthFailure(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	c, err := quic.DialAddr(ctx, ln.ln.Addr().String(), ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AcceptStream(ctx); err != nil {
		t.Fatal(err)
	}
	_ = c.CloseWithError(closeHandshakeFailed, "handshake failed")

	_, err = ln.Accept(ctx)
	if !errors.Is(err, ErrHandshakeFailed) {
		t.Fatalf("got %v, want ErrHandshakeFailed", err)
	}
	if errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("an early close counted as a failed PAKE: %v", err)
	}
}
