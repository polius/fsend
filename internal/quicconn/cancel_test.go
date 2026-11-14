package quicconn

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/quic-go/quic-go"
)

// dropOnce drops the first datagram written after armed is set.
type dropOnce struct {
	net.PacketConn
	armed, dropped atomic.Bool
}

func (d *dropOnce) WriteTo(p []byte, addr net.Addr) (int, error) {
	if d.armed.Load() && d.dropped.CompareAndSwap(false, true) {
		return len(p), nil
	}
	return d.PacketConn.WriteTo(p, addr)
}

// A cancel survives losing its close packet: the sender (whose process
// exits right after) lingers long enough to resend it when the
// receiver's next packet arrives. Without the linger the receiver only
// learns at its idle timeout, as a drop rather than a cancel (seen in CI
// as a sender Ctrl-C the receiver sat through for 15 s).
func TestAcceptResult_CancelSurvivesALostClose(t *testing.T) {
	const secret = "abc-defg-hjk"
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	lossy := &dropOnce{PacketConn: udp}
	tr := &quic.Transport{Conn: lossy}
	tlsCfg, err := SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	qln, err := tr.Listen(tlsCfg, QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	ln := NewListener(qln, secret)
	defer func() { _ = ln.Close(); _ = tr.Close(); _ = udp.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	recv := make(chan *AcceptResult, 1)
	go func() {
		res, err := Dial(ctx, udp.LocalAddr().String(), secret)
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
	// A receiver mid-transfer keeps sending (acks, control); stand in with
	// writes on its control stream.
	go func() {
		for r.Conn.Context().Err() == nil {
			_, _ = r.Streams.Control.Write([]byte{0})
			time.Sleep(time.Millisecond) // well inside quic-go's 3·PTO resend window
		}
	}()

	lossy.armed.Store(true)
	snd.Cancel()
	_ = ln.Close()
	_ = tr.Close() // the sender's process is gone
	if !lossy.dropped.Load() {
		t.Fatal("the close packet was never sent")
	}
	if !r.PeerCancelled(3 * time.Second) {
		t.Fatalf("a lost close left the receiver unaware of the cancel; cause: %v", context.Cause(r.Conn.Context()))
	}
}
