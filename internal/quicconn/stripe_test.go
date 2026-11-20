package quicconn

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// pipeLane joins a laneOut to a laneIn in memory.
type pipeLane struct {
	r *io.PipeReader
	w *io.PipeWriter
}

func newPipeLane() *pipeLane {
	r, w := io.Pipe()
	return &pipeLane{r, w}
}

func (p *pipeLane) Write(b []byte) (int, error)      { return p.w.Write(b) }
func (p *pipeLane) Close() error                     { return p.w.Close() }
func (p *pipeLane) SetWriteDeadline(time.Time) error { return nil }
func (p *pipeLane) CancelWrite()                     { p.w.CloseWithError(errors.New("cancelled")) }
func (p *pipeLane) Read(b []byte) (int, error)       { return p.r.Read(b) }
func (p *pipeLane) CancelRead()                      { p.r.CloseWithError(errors.New("cancelled")) }
func (p *pipeLane) SetReadDeadline(time.Time) error  { return nil }

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

// Stripes spread over lanes come back in order, however the lanes are
// paced, and the stream ends exactly at the stripe count.
func TestStripe_RoundTripOverLanes(t *testing.T) {
	lanes := []*pipeLane{newPipeLane(), newPipeLane(), newPipeLane()}
	out := newStripeOut(lanes[0])
	in := newStripeIn(lanes[0])
	for _, l := range lanes[1:] {
		in.expectLane()
		out.addLane(l)
		in.attachLane(l)
	}
	data := randomBytes(5*stripeSize + 1234)
	var wg sync.WaitGroup
	wg.Go(func() {
		// Engine-like writes: small headers between large payloads.
		for off := 0; off < len(data); {
			n := min(len(data)-off, 100_000)
			if _, err := out.Write([]byte{byte(off)}); err != nil {
				t.Error(err)
			}
			if _, err := out.Write(data[off : off+n]); err != nil {
				t.Error(err)
			}
			off += n
		}
		if err := out.Close(); err != nil {
			t.Error(err)
		}
	})
	got, err := io.ReadAll(in)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	var want []byte
	for off := 0; off < len(data); {
		n := min(len(data)-off, 100_000)
		want = append(want, byte(off))
		want = append(want, data[off:off+n]...)
		off += n
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("got %d bytes, want %d (equal %v)", len(got), len(want), bytes.Equal(got, want))
	}
}

// A stream whose lanes all end before the stripe count arrived is
// truncated, not complete.
func TestStripe_EndWithoutCountIsAnError(t *testing.T) {
	lane := newPipeLane()
	in := newStripeIn(lane)
	go func() {
		_ = lane.Close() // no stripes, no end frame
	}()
	if _, err := io.ReadAll(in); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("err = %v, want unexpected EOF", err)
	}
}

// A Close while a Write is blocked (every lane busy) aborts the Write
// instead of hanging.
func TestStripe_CloseDuringBlockedWriteAborts(t *testing.T) {
	lane := newPipeLane() // nobody reads it: the lane blocks
	out := newStripeOut(lane)
	errc := make(chan error, 1)
	go func() {
		_, err := out.Write(randomBytes(10 * stripeSize))
		errc <- err
	}()
	time.Sleep(50 * time.Millisecond)
	done := make(chan struct{})
	go func() {
		_ = out.Close()
		close(done)
	}()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("blocked Write returned no error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked Write did not return after Close")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung")
	}
}

// stripedPair is a sender listener and a receiver dialing it, each on its
// own quic.Transport, as on the direct route.
func stripedPair(t *testing.T, secret string) (*Listener, *quic.Transport, *quic.Conn) {
	t.Helper()
	spc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	str := NewTransport(spc)
	tlsCfg, err := SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	qln, err := str.Listen(tlsCfg, QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	ln := NewListener(qln, secret)
	rpc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rtr := NewTransport(rpc)
	t.Cleanup(func() {
		_ = ln.Close()
		_ = str.Close()
		_ = rtr.Close()
		_ = spc.Close()
		_ = rpc.Close()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := rtr.Dial(ctx, spc.LocalAddr(), ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	return ln, rtr, c
}

// On the direct route a striped session opens its extra lanes and the
// data arrives whole across them.
func TestStripe_LanesAttachAndCarryTheData(t *testing.T) {
	const secret = "abc-defg-jkm"
	ln, rtr, c := stripedPair(t, secret)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type result struct {
		res *AcceptResult
		err error
	}
	rc := make(chan result, 1)
	go func() {
		res, err := ReceiverHandshakeOver(ctx, rtr, c, secret)
		rc <- result{res, err}
	}()
	sres, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sres.Close()
	r := <-rc
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.res.Close()
	if !striped(c) {
		t.Fatal("not striped")
	}
	// Wait for the lanes to attach on the sender.
	deadline := time.Now().Add(5 * time.Second)
	for {
		ln.mu.Lock()
		n := 0
		for _, s := range ln.laneSessions {
			n = s.lanes
		}
		ln.mu.Unlock()
		if n == laneCount()-1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lanes attached = %d, want %d", n, laneCount()-1)
		}
		time.Sleep(10 * time.Millisecond)
	}
	data := randomBytes(3<<20 + 77)
	go func() {
		_, _ = sres.Streams.Data.Write(data)
		_ = sres.Streams.Data.Close()
	}()
	got, err := io.ReadAll(r.res.Streams.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("got %d bytes, want %d", len(got), len(data))
	}
}

// A lane that doesn't know the session's key is refused, and isn't
// counted against the session.
func TestStripe_ForgedLaneIsRefused(t *testing.T) {
	const secret = "abc-defg-jkm"
	ln, rtr, c := stripedPair(t, secret)
	t.Setenv("FSEND_STRIPE_LANES", "1") // only the forged lane below
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go func() { _, _ = ReceiverHandshakeOver(ctx, rtr, c, secret) }()
	sres, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sres.Close()
	ln.mu.Lock()
	var id [laneIDLen]byte
	for k := range ln.laneSessions {
		id = k
	}
	ln.mu.Unlock()
	// The id is right (as if it had leaked), the key is not.
	si := newStripeIn(newPipeLane())
	si.expectLane()
	err = dialLane(rtr, c, randomBytes(32), id, 1, si)
	if err == nil {
		t.Fatal("forged lane attached")
	}
	ln.mu.Lock()
	n := ln.laneSessions[id].lanes
	ln.mu.Unlock()
	if n != 0 {
		t.Fatalf("a refused lane counted against the session: %d", n)
	}
}
