package quicconn

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/pake"
)

// authPair is a QUIC connection with the control stream open on both ends:
// the sender end opened it, as SenderHandshake does.
type authPair struct {
	senderConn, receiverConn *quic.Conn
	sender, receiver         *quic.Stream
}

// newAuthPair connects two ends on loopback. The sender's stream only
// reaches the receiver once the sender writes, so the receiver end is
// accepted lazily by receiverStream.
func newAuthPair(t *testing.T) *authPair {
	t.Helper()
	tlsSrv, err := SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr("127.0.0.1:0", tlsSrv, QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	rc, err := quic.DialAddr(ctx, ln.Addr().String(), ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	sc, err := ln.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ss, err := sc.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p := &authPair{senderConn: sc, receiverConn: rc, sender: ss}
	t.Cleanup(func() {
		_ = sc.CloseWithError(0, "")
		_ = rc.CloseWithError(0, "")
	})
	return p
}

// receiverStream accepts the control stream on the receiver end. Call it
// after the sender end has written (its PAKE share).
func (p *authPair) receiverStream(t *testing.T) *quic.Stream {
	t.Helper()
	if p.receiver == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		s, err := p.receiverConn.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		p.receiver = s
	}
	return p.receiver
}

// rawPeer is a hand-rolled handshake end: it exchanges PAKE shares like
// authenticatePeer and then leaves the confirmation tags to the test.
type rawPeer struct {
	myTag, wantTag []byte
}

// rawShares runs the share exchange for role r with secret. open returns
// the control stream: a sender has it already, a receiver accepts it once
// the sender's share is on the wire.
func rawShares(t *testing.T, st tls.ConnectionState, open func() io.ReadWriter, secret string, r role) (*rawPeer, io.ReadWriter) {
	t.Helper()
	exp, err := st.ExportKeyingMaterial(exporterLabel, nil, exporterLen)
	if err != nil {
		t.Fatal(err)
	}
	myAD, peerAD, pr := adSender, adReceiver, pake.Initiator
	if r == roleReceiver {
		myAD, peerAD, pr = adReceiver, adSender, pake.Responder
	}
	ps, err := pake.New(pr, pake.Params{PRS: []byte(secret), CI: []byte("fsend"), SID: exp, AD: myAD})
	if err != nil {
		t.Fatal(err)
	}
	myMsg := append([]byte{suiteCPace, suiteVersion}, ps.Share()...)
	var ctrl io.ReadWriter
	if r == roleSender {
		ctrl = open()
		if err := writeFramed(ctrl, myMsg); err != nil {
			t.Fatal(err)
		}
	}
	if r == roleReceiver {
		ctrl = open()
	}
	peerMsg, err := readFramed(ctrl, maxPakeMsgLen)
	if err != nil {
		t.Fatal(err)
	}
	if r == roleReceiver {
		if err := writeFramed(ctrl, myMsg); err != nil {
			t.Fatal(err)
		}
	}
	share, err := parsePakeMsg(peerMsg)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ps.Finish(share, peerAD)
	if err != nil {
		t.Fatal(err)
	}
	senderMsg, receiverMsg := myMsg, peerMsg
	if r == roleReceiver {
		senderMsg, receiverMsg = peerMsg, myMsg
	}
	return &rawPeer{
		myTag:   confirmTag(key, exp, r, senderMsg, receiverMsg),
		wantTag: confirmTag(key, exp, otherRole(r), senderMsg, receiverMsg),
	}, ctrl
}

// A peer that never learned the code must not be able to authenticate by
// reflecting the honest side's confirmation tag back at it. Direction
// separation in confirmTag is what closes this; this test guards it. The
// receiver is the side that sends its tag first, so it is the one a
// reflection can be tried on.
func TestHandshake_TagReflectionRejected(t *testing.T) {
	const honestCode = "abc-defg-jkm"
	const attackerCode = "zzz-zzzz-zzz"

	p := newAuthPair(t)
	honestErr := make(chan error, 1)
	go func() {
		// The honest receiver: it accepts the stream once the fake
		// sender's share arrives.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		ctrl, err := p.receiverConn.AcceptStream(ctx)
		if err != nil {
			honestErr <- err
			return
		}
		_ = ctrl.SetReadDeadline(time.Now().Add(10 * time.Second))
		_, err = authenticatePeer(p.receiverConn.ConnectionState().TLS, ctrl, honestCode, roleReceiver)
		honestErr <- err
	}()

	// Malicious sender: a well-formed CPace round with the wrong code (so
	// the honest Finish succeeds), then the honest tag reflected.
	_, ctrl := rawShares(t, p.senderConn.ConnectionState().TLS, func() io.ReadWriter { return p.sender }, attackerCode, roleSender)
	var tag [tagLen]byte
	if _, err := io.ReadFull(ctrl, tag[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := ctrl.Write(tag[:]); err != nil {
		t.Fatal(err)
	}

	if err := <-honestErr; !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("reflected tag was accepted (got %v); auth bypass", err)
	}
}
