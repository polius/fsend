package quicconn

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
)

// The tests here pin the order of the confirmation tags: the receiver's
// goes first, and the sender shows its own only to a peer whose tag was
// right. A tag is a one-guess test of the code for whoever reads it, so a
// sender that answered first would hand that test to any peer that
// connected, uncounted when the peer then left without answering.

// failsRun reads the listener's count of failed PAKEs.
func failsRun(l *Listener) (n int, penalised bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.failsRun, !l.nextStart.IsZero()
}

// dialRawQUIC connects to a QUIC listener as a hand-rolled receiver that
// guesses code, and returns it after the share exchange.
func dialRawQUIC(t *testing.T, addr, guess string) (*rawPeer, *quic.Conn, *quic.Stream) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := quic.DialAddr(ctx, addr, ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.CloseWithError(0, "") })
	var ctrl *quic.Stream
	peer, _ := rawShares(t, c.ConnectionState().TLS, func() io.ReadWriter {
		s, err := c.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		ctrl = s
		return s
	}, guess, roleReceiver)
	return peer, c, ctrl
}

// A peer that withholds its tag gets nothing from the sender: not the
// sender's tag, so no test of its guess, whether the guess was right or
// wrong. Having learned nothing, it is not a failed attempt either.
func TestSender_ShowsNoTagToAPeerThatWithholdsIts(t *testing.T) {
	const real = "abc-defg-hjk"
	for _, guess := range []string{"xyz-pqrs-tuv", real} {
		t.Run(guess, func(t *testing.T) {
			ln, err := ListenAddr("127.0.0.1:0", real)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			_, c, ctrl := dialRawQUIC(t, ln.ln.Addr().String(), guess)

			_ = ctrl.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
			var b [1]byte
			if n, err := ctrl.Read(b[:]); n != 0 || err == nil {
				t.Fatalf("the sender wrote %d byte(s) before it had our tag (err %v)", n, err)
			}
			_ = c.CloseWithError(0, "")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = ln.Accept(ctx)
			if !errors.Is(err, ErrHandshakeFailed) {
				t.Fatalf("got %v, want ErrHandshakeFailed", err)
			}
			if errors.Is(err, fserrors.ErrPeerAuthFailed) {
				t.Fatalf("a peer that learned nothing counted as a failed PAKE: %v", err)
			}
			if n, pen := failsRun(ln); n != 0 || pen {
				t.Fatalf("failsRun = %d, penalised = %v; want none", n, pen)
			}
		})
	}
}

// A wrong tag is a failed PAKE: counted, penalised, and answered with a
// close in place of the sender's tag.
func TestSender_CountsAWrongTagAndAnswersWithAClose(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	peer, _, ctrl := dialRawQUIC(t, ln.ln.Addr().String(), "xyz-pqrs-tuv")
	if _, err := ctrl.Write(peer.myTag); err != nil {
		t.Fatal(err)
	}
	_ = ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [1]byte
	n, err := ctrl.Read(b[:])
	if n != 0 || !peerRejected(err) {
		t.Fatalf("after a wrong tag: read %d byte(s), err %v; want the handshake-failed close and no tag", n, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = ln.Accept(ctx); !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("got %v, want ErrPeerAuthFailed", err)
	}
	if n, pen := failsRun(ln); n != 1 || !pen {
		t.Fatalf("failsRun = %d, penalised = %v; want 1 and a penalty", n, pen)
	}
}

// The same holds on the TCP route, which shares the listener's count.
func TestTCP_SenderShowsNoTagToAPeerThatWithholdsIts(t *testing.T) {
	const real = "abc-defg-hjk"
	for _, guess := range []string{"xyz-pqrs-tuv", real} {
		t.Run(guess, func(t *testing.T) {
			ln, addr := tcpListener(t, real)
			nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			tc := tls.Client(nc, tcpTLS(ReceiverTLSConfig()))
			defer tc.Close()
			if err := tc.Handshake(); err != nil {
				t.Fatal(err)
			}
			if _, err := tc.Write([]byte{tcpRoleControl}); err != nil {
				t.Fatal(err)
			}
			conn := newTCPConn(tc)
			ctrl := &tcpCtrlStream{c: conn}
			rawShares(t, tc.ConnectionState(), func() io.ReadWriter { return ctrl }, guess, roleReceiver)

			_ = ctrl.SetReadDeadline(time.Now().Add(700 * time.Millisecond))
			var b [1]byte
			if n, err := ctrl.Read(b[:]); n != 0 || err == nil {
				t.Fatalf("the sender wrote %d byte(s) before it had our tag (err %v)", n, err)
			}
			_ = conn.CloseWithError(0, "")

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = ln.Accept(ctx)
			if !errors.Is(err, ErrHandshakeFailed) || errors.Is(err, fserrors.ErrPeerAuthFailed) {
				t.Fatalf("got %v, want a handshake failure that is not a failed PAKE", err)
			}
			if n, pen := failsRun(ln); n != 0 || pen {
				t.Fatalf("failsRun = %d, penalised = %v; want none", n, pen)
			}
		})
	}
}

// A wrong tag on the TCP route is counted and answered with a close, and
// not one byte of the sender's tag. This is the route where the order
// shows: TCP delivers what was written before a close, where a QUIC close
// may drop it, so a sender that wrote its tag and then checked ours would
// be caught here and could pass the QUIC test above.
func TestTCP_SenderCountsAWrongTagAndAnswersWithAClose(t *testing.T) {
	ln, addr := tcpListener(t, "abc-defg-hjk")
	nc, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	tc := tls.Client(nc, tcpTLS(ReceiverTLSConfig()))
	defer tc.Close()
	if err := tc.Handshake(); err != nil {
		t.Fatal(err)
	}
	if _, err := tc.Write([]byte{tcpRoleControl}); err != nil {
		t.Fatal(err)
	}
	conn := newTCPConn(tc)
	ctrl := &tcpCtrlStream{c: conn}
	peer, _ := rawShares(t, tc.ConnectionState(), func() io.ReadWriter { return ctrl }, "xyz-pqrs-tuv", roleReceiver)
	if _, err := ctrl.Write(peer.myTag); err != nil {
		t.Fatal(err)
	}

	_ = ctrl.SetReadDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(ctrl)
	if len(got) != 0 || !peerRejected(err) {
		t.Fatalf("after a wrong tag: read %d byte(s), err %v; want the handshake-failed close and no tag", len(got), err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err = ln.Accept(ctx); !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("got %v, want ErrPeerAuthFailed", err)
	}
	if n, pen := failsRun(ln); n != 1 || !pen {
		t.Fatalf("failsRun = %d, penalised = %v; want 1 and a penalty", n, pen)
	}
}

// A receiver whose tag went unanswered reports ErrUnconfirmed, whichever
// way the sender left: the peer may have used the tag to test a guess.
// It is not a wrong code (a dropped link looks the same).
func TestReceiver_UnansweredTagIsUnconfirmed(t *testing.T) {
	const code = "abc-defg-hjk"
	for _, leave := range []string{"close", "stall"} {
		t.Run(leave, func(t *testing.T) {
			p := newAuthPair(t)
			recvErr := make(chan error, 1)
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				ctrl, err := p.receiverConn.AcceptStream(ctx)
				if err != nil {
					recvErr <- err
					return
				}
				_ = ctrl.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
				_, err = authenticatePeer(p.receiverConn.ConnectionState().TLS, ctrl, code, roleReceiver)
				recvErr <- err
			}()
			// A fake sender that holds the right code, reads the
			// receiver's tag (its one test) and never answers.
			peer, ctrl := rawShares(t, p.senderConn.ConnectionState().TLS, func() io.ReadWriter { return p.sender }, code, roleSender)
			var tag [tagLen]byte
			if _, err := io.ReadFull(ctrl, tag[:]); err != nil {
				t.Fatal(err)
			}
			if string(tag[:]) != string(peer.wantTag) {
				t.Fatal("the receiver's tag is not the one the transcript gives")
			}
			if leave == "close" {
				_ = p.senderConn.CloseWithError(0, "")
			}
			err := <-recvErr
			if !errors.Is(err, ErrUnconfirmed) {
				t.Fatalf("got %v, want ErrUnconfirmed", err)
			}
			if errors.Is(err, fserrors.ErrPeerAuthFailed) {
				t.Fatalf("an unanswered tag reported as a wrong code: %v", err)
			}
		})
	}
}

// legacyAuthenticate is the tag exchange as releases up to 1.16 ran it:
// either role wrote its tag first and then read the peer's.
func legacyAuthenticate(t *testing.T, st tls.ConnectionState, open func() io.ReadWriter, secret string, r role) error {
	peer, ctrl := rawShares(t, st, open, secret, r)
	if _, err := ctrl.Write(peer.myTag); err != nil {
		return fmt.Errorf("send tag: %w", err)
	}
	var got [tagLen]byte
	if _, err := io.ReadFull(ctrl, got[:]); err != nil {
		return fmt.Errorf("recv tag: %w", err)
	}
	if string(got[:]) != string(peer.wantTag) {
		return fserrors.ErrPeerAuthFailed
	}
	return nil
}

// Both roles still complete with a peer that writes its tag before
// reading, as every release up to 1.16 does.
func TestHandshake_InteroperatesWithWriteFirstPeers(t *testing.T) {
	const code = "abc-defg-hjk"
	t.Run("old receiver, new sender", func(t *testing.T) {
		p := newAuthPair(t)
		newErr := make(chan error, 1)
		go func() {
			_ = p.sender.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, err := authenticatePeer(p.senderConn.ConnectionState().TLS, p.sender, code, roleSender)
			newErr <- err
		}()
		err := legacyAuthenticate(t, p.receiverConn.ConnectionState().TLS, func() io.ReadWriter { return p.receiverStream(t) }, code, roleReceiver)
		if err != nil {
			t.Fatalf("old receiver: %v", err)
		}
		if err := <-newErr; err != nil {
			t.Fatalf("new sender: %v", err)
		}
	})
	t.Run("old sender, new receiver", func(t *testing.T) {
		p := newAuthPair(t)
		newErr := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			ctrl, err := p.receiverConn.AcceptStream(ctx)
			if err != nil {
				newErr <- err
				return
			}
			_ = ctrl.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, err = authenticatePeer(p.receiverConn.ConnectionState().TLS, ctrl, code, roleReceiver)
			newErr <- err
		}()
		err := legacyAuthenticate(t, p.senderConn.ConnectionState().TLS, func() io.ReadWriter { return p.sender }, code, roleSender)
		if err != nil {
			t.Fatalf("old sender: %v", err)
		}
		if err := <-newErr; err != nil {
			t.Fatalf("new receiver: %v", err)
		}
	})
	t.Run("old receiver with a wrong code", func(t *testing.T) {
		p := newAuthPair(t)
		newErr := make(chan error, 1)
		go func() {
			_ = p.sender.SetReadDeadline(time.Now().Add(10 * time.Second))
			_, err := authenticatePeer(p.senderConn.ConnectionState().TLS, p.sender, code, roleSender)
			if err != nil {
				_ = p.senderConn.CloseWithError(closeHandshakeFailed, "handshake failed")
			}
			newErr <- err
		}()
		err := legacyAuthenticate(t, p.receiverConn.ConnectionState().TLS, func() io.ReadWriter { return p.receiverStream(t) }, "xyz-pqrs-tuv", roleReceiver)
		if err == nil {
			t.Fatal("an old receiver with a wrong code completed the handshake")
		}
		if err := <-newErr; !errors.Is(err, fserrors.ErrPeerAuthFailed) {
			t.Fatalf("new sender: got %v, want ErrPeerAuthFailed", err)
		}
	})
}

// DialTCP reports an unanswered tag as ErrUnconfirmed, which is what
// keeps the caller from offering the same address a second handshake.
func TestTCP_DialReportsAnUnansweredTag(t *testing.T) {
	const code = "abc-defg-hjk"
	srvTLS, err := SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	tl, err := tls.Listen("tcp", "127.0.0.1:0", tcpTLS(srvTLS))
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	gotTag := make(chan bool, 1)
	go func() {
		nc, err := tl.Accept()
		if err != nil {
			return
		}
		tc := nc.(*tls.Conn)
		defer tc.Close()
		var role [1]byte
		if _, err := io.ReadFull(tc, role[:]); err != nil || role[0] != tcpRoleControl {
			gotTag <- false
			return
		}
		conn := newTCPConn(tc)
		ctrl := &tcpCtrlStream{c: conn}
		// A fake sender that takes the receiver's tag and leaves.
		peer, _ := rawShares(t, tc.ConnectionState(), func() io.ReadWriter { return ctrl }, "xyz-pqrs-tuv", roleSender)
		var tag [tagLen]byte
		_, err = io.ReadFull(ctrl, tag[:])
		gotTag <- err == nil && string(tag[:]) != string(peer.wantTag)
		_ = conn.CloseWithError(0, "")
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = DialTCP(ctx, tl.Addr().String(), code, 2*time.Second)
	if !<-gotTag {
		t.Fatal("the fake sender did not get the receiver's tag")
	}
	if !errors.Is(err, ErrUnconfirmed) || errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("got %v, want ErrUnconfirmed and not a failed PAKE", err)
	}
}
