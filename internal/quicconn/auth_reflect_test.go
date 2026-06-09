package quicconn

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/pake"
)

// A peer that never learned the code must not be able to authenticate by
// reflecting the honest side's confirmation tag back at it. Direction
// separation in confirmTag is what closes this; this test guards it.
func TestHandshake_TagReflectionRejected(t *testing.T) {
	const honestCode = "abc-defg-jkm"
	const attackerCode = "zzz-zzzz-zzz"

	tlsSrv, err := SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := quic.ListenAddr("127.0.0.1:0", tlsSrv, QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	honestErr := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := ln.Accept(ctx)
		if err != nil {
			honestErr <- err
			return
		}
		ctrl, err := c.OpenStreamSync(ctx)
		if err != nil {
			honestErr <- err
			return
		}
		honestErr <- authenticatePeer(c, ctrl, honestCode, roleSender)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := quic.DialAddr(ctx, ln.Addr().String(), ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	ctrl, err := c.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Malicious receiver: complete a valid SPAKE2 round with the wrong
	// code (so the honest Finish succeeds), then reflect the honest tag.
	if _, err := readReflectFrame(ctrl); err != nil {
		t.Fatal(err)
	}
	if err := writeReflectFrame(ctrl, pake.New(attackerCode).Start()); err != nil {
		t.Fatal(err)
	}
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

func writeReflectFrame(w io.Writer, msg []byte) error {
	var hdr [2]byte
	binary.BigEndian.PutUint16(hdr[:], uint16(len(msg)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(msg)
	return err
}

func readReflectFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	buf := make([]byte, binary.BigEndian.Uint16(hdr[:]))
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
