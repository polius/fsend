package quicconn

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/wire"
)

// tcpListener is a QUIC listener serving the TCP route on the same port.
func tcpListener(t *testing.T, secret string) (*Listener, string) {
	t.Helper()
	ln, err := ListenAddr("127.0.0.1:0", secret)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	tl, err := net.Listen("tcp", ln.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.ServeTCP(tl); err != nil {
		t.Fatal(err)
	}
	return ln, tl.Addr().String()
}

// pairTCP dials the TCP route and accepts it.
func pairTCP(t *testing.T, ln *Listener, addr, secret string) (snd, rcv *AcceptResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type out struct {
		res *AcceptResult
		err error
	}
	ch := make(chan out, 1)
	go func() {
		r, err := DialTCP(ctx, addr, secret, time.Second)
		ch <- out{r, err}
	}()
	s, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	o := <-ch
	if o.err != nil {
		t.Fatalf("DialTCP: %v", o.err)
	}
	t.Cleanup(func() { s.Close(); o.res.Close() })
	return s, o.res
}

// A whole transfer runs over the TCP route, through the same engine.
func TestTCP_LoopbackTransfer(t *testing.T) {
	const secret = "abc-defg-hjk"
	src, dst := t.TempDir(), t.TempDir()
	payload := make([]byte, 6*1024*1024+123)
	_, _ = rand.Read(payload)
	if err := os.WriteFile(filepath.Join(src, "payload.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	items, err := transfer.Walk([]string{filepath.Join(src, "payload.bin")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, addr := tcpListener(t, secret)
	snd, rcv := pairTCP(t, ln, addr, secret)
	if _, ok := snd.Conn.(*tcpConn); !ok {
		t.Fatalf("the sender's connection is %T, want the TCP route", snd.Conn)
	}
	if !bytes.Equal(snd.Streams.Binding, rcv.Streams.Binding) || len(snd.Streams.Binding) == 0 {
		t.Fatal("the two sides derived different session bindings")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var sendErr, recvErr error
	wg.Add(2)
	go func() {
		defer wg.Done()
		sendErr = transfer.Send(ctx, &snd.Streams, transfer.SendOptions{Mode: wire.ModeFiles, Sources: items, Hostname: "alice"})
	}()
	go func() {
		defer wg.Done()
		recvErr = transfer.Recv(ctx, &rcv.Streams, transfer.RecvOptions{Hostname: "bob", TargetDir: dst})
	}()
	wg.Wait()
	if sendErr != nil || recvErr != nil {
		t.Fatalf("send=%v recv=%v", sendErr, recvErr)
	}
	got, err := os.ReadFile(filepath.Join(dst, "payload.bin"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("received file differs (err %v)", err)
	}
}

// A wrong code fails the PAKE on both sides, as on QUIC, and counts as a
// failed handshake on the listener.
func TestTCP_WrongCodeRejected(t *testing.T) {
	ln, addr := tcpListener(t, "abc-defg-hjk")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := DialTCP(ctx, addr, "xyz-pqrs-tuv", time.Second)
	if !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("receiver: %v, want ErrPeerAuthFailed", err)
	}
	if _, err := ln.Accept(ctx); !errors.Is(err, ErrHandshakeFailed) || !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("listener: %v, want a failed PAKE", err)
	}
}

// A cancel reaches the peer as one, so it reports E032 instead of
// retrying, and a plain close does not look like a cancel.
func TestTCP_CancelReachesPeer(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, addr := tcpListener(t, secret)
	snd, rcv := pairTCP(t, ln, addr, secret)
	if rcv.PeerCancelled(20 * time.Millisecond) {
		t.Fatal("PeerCancelled before any close")
	}
	snd.Cancel()
	if !rcv.PeerCancelled(2 * time.Second) {
		t.Fatalf("the cancel was not seen: %v", context.Cause(rcv.Conn.Context()))
	}

	snd2, rcv2 := pairTCP(t, ln, addr, secret)
	snd2.Close()
	if rcv2.PeerCancelled(500 * time.Millisecond) {
		t.Fatal("a plain close read as a cancel")
	}
	if rcv2.Conn.Context().Err() == nil {
		t.Fatal("the plain close did not end the receiver's connection")
	}
}

// The control stream's end (Close) is not the connection's: the peer
// reads EOF and can still answer, as with a QUIC stream FIN.
func TestTCP_StreamEndKeepsConnection(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, addr := tcpListener(t, secret)
	snd, rcv := pairTCP(t, ln, addr, secret)
	if _, err := snd.Streams.Control.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	_ = snd.Streams.Control.Close()
	buf := make([]byte, 16)
	n, _ := rcv.Streams.Control.Read(buf)
	if string(buf[:n]) != "bye" {
		t.Fatalf("read %q", buf[:n])
	}
	if _, err := rcv.Streams.Control.Read(buf); err == nil {
		t.Fatal("no EOF after the peer's stream end")
	}
	if _, err := rcv.Streams.Control.Write([]byte("ok")); err != nil {
		t.Fatalf("answer after the peer's FIN: %v", err)
	}
	n, err := snd.Streams.Control.Read(buf)
	if err != nil || string(buf[:n]) != "ok" {
		t.Fatalf("the sender read %q, %v", buf[:n], err)
	}
}

// A read deadline on the control stream times out and can be lifted: it
// lives above the TLS connection, which a timed-out write would ruin.
func TestTCP_ControlDeadlines(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, addr := tcpListener(t, secret)
	snd, rcv := pairTCP(t, ln, addr, secret)
	type deadliner interface{ SetReadDeadline(time.Time) error }
	_ = rcv.Streams.Control.(deadliner).SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := rcv.Streams.Control.Read(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("read past the deadline: %v", err)
	}
	type wdeadliner interface{ SetWriteDeadline(time.Time) error }
	_ = snd.Streams.Control.(wdeadliner).SetWriteDeadline(time.Now().Add(-time.Second))
	if _, err := snd.Streams.Control.Write([]byte("x")); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("write past the deadline: %v", err)
	}
	// Both lifted: the streams still work.
	_ = rcv.Streams.Control.(deadliner).SetReadDeadline(time.Time{})
	_ = snd.Streams.Control.(wdeadliner).SetWriteDeadline(time.Time{})
	if _, err := snd.Streams.Control.Write([]byte("y")); err != nil {
		t.Fatalf("write after lifting the deadline: %v", err)
	}
	buf := make([]byte, 1)
	if _, err := rcv.Streams.Control.Read(buf); err != nil || buf[0] != 'y' {
		t.Fatalf("read after lifting the deadline: %q %v", buf, err)
	}
}

// A data connection that names no session, or presents a tag that isn't
// the session's, gets nothing.
func TestTCP_DataConnectionMustBelongToTheSession(t *testing.T) {
	ln, addr := tcpListener(t, "abc-defg-hjk")
	dialData := func(id []byte) *tls.Conn {
		nc, err := net.DialTimeout("tcp", addr, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		tc := tls.Client(nc, tcpTLS(ReceiverTLSConfig()))
		if err := tc.Handshake(); err != nil {
			t.Fatal(err)
		}
		_, _ = tc.Write(append([]byte{tcpRoleData}, id...))
		return tc
	}
	tc := dialData(make([]byte, 32))
	_ = tc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := tc.Read(make([]byte, 64)); err == nil {
		t.Fatalf("an unknown session id got %d bytes back", n)
	}
	_ = tc.Close()
	_ = ln
}

// The listener's peer filter applies to the TCP route too.
func TestTCP_PeerFilter(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk", WithPeerFilter(func(net.Addr) bool { return false }))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	tl, err := net.Listen("tcp", ln.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if err := ln.ServeTCP(tl); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := DialTCP(ctx, tl.Addr().String(), "abc-defg-hjk", time.Second); err == nil {
		t.Fatal("a filtered peer paired over TCP")
	}
}

// A sender without the TCP route (an older release) refuses the connect
// at once, so the caller's fallback to QUIC costs nothing.
func TestTCP_NoRouteFailsFast(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	start := time.Now()
	if _, err := DialTCP(context.Background(), ln.ln.Addr().String(), "abc-defg-hjk", 300*time.Millisecond); err == nil {
		t.Fatal("dialed a TCP route nobody serves")
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("the failed dial took %v", d)
	}
}

// A peer that disappears without closing (a closed lid) ends the
// connection at the idle timeout, as QUIC's would.
func TestTCP_SilentPeerTimesOut(t *testing.T) {
	t.Setenv("FSEND_QUIC_IDLE_TIMEOUT", "600ms")
	const secret = "abc-defg-hjk"
	ln, addr := tcpListener(t, secret)
	snd, rcv := pairTCP(t, ln, addr, secret)
	// Silence the receiver without a close: no more pings or frames.
	rc := rcv.Conn.(*tcpConn)
	rc.cancel(errors.New("test: gone silent"))
	select {
	case <-snd.Conn.Context().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("a silent peer kept the connection alive")
	}
}

// When the receiver closes mid-transfer, the sender's next data write
// fails with the receiver's close, as over QUIC, so the sender re-pairs
// at once instead of retrying a route nobody will re-dial.
func TestTCP_PeerCloseExplainsDataErrors(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, addr := tcpListener(t, secret)
	snd, rcv := pairTCP(t, ln, addr, secret)
	rcv.Close()
	buf := make([]byte, 1<<20)
	var err error
	deadline := time.Now().Add(5 * time.Second)
	for err == nil && time.Now().Before(deadline) {
		_, err = snd.Streams.Data.Write(buf)
	}
	var appErr *quic.ApplicationError
	if !errors.As(err, &appErr) || !appErr.Remote {
		t.Fatalf("data write after the receiver closed: %v, want the receiver's close", err)
	}
}

// A close returns promptly even while a control write is stuck on a peer
// that stopped reading (the Ctrl-C case), and wakes a pending control
// Read on this side, as on QUIC.
func TestTCP_CloseIsPromptAndWakesReaders(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, addr := tcpListener(t, secret)
	snd, rcv := pairTCP(t, ln, addr, secret)
	_ = rcv // never reads: its buffers fill, then the sender's writes block

	readErr := make(chan error, 1)
	go func() {
		_, err := snd.Streams.Control.Read(make([]byte, 1))
		readErr <- err
	}()
	go func() {
		buf := make([]byte, 1<<20)
		for {
			if _, err := snd.Streams.Control.Write(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond) // long enough to fill the buffers

	done := make(chan struct{})
	go func() { _ = snd.Conn.CloseWithError(0, ""); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("CloseWithError hung behind a stuck control write")
	}
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("the pending Read returned no error after the close")
		}
	case <-time.After(time.Second):
		t.Fatal("a local close left a control Read blocked")
	}
}

// A local close wakes a control Read pending on an idle connection.
func TestTCP_LocalCloseWakesIdleReader(t *testing.T) {
	const secret = "abc-defg-hjk"
	ln, addr := tcpListener(t, secret)
	snd, _ := pairTCP(t, ln, addr, secret)
	readErr := make(chan error, 1)
	go func() {
		_, err := snd.Streams.Control.Read(make([]byte, 1))
		readErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = snd.Conn.CloseWithError(0, "")
	select {
	case err := <-readErr:
		if err == nil {
			t.Fatal("the pending Read returned no error after the close")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("a local close left a control Read blocked")
	}
}
