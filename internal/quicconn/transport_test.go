package quicconn

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net"
	"testing"
	"time"
)

// TestNewTransportRealHandshake guards the production wiring: that quic-go
// accepts our 8-byte ConnectionIDGenerator and that a real handshake +
// 1-RTT transfer completes through NewTransport on both ends (not
// quicconn.Dial/ListenAddr's self-opened sockets). The STUN-safety
// property itself is proven separately in connid_test.go.
func TestNewTransportRealHandshake(t *testing.T) {
	const code = "abc-defg-jkm"

	// quic.Transport.Close only closes conns it opened itself; we supplied
	// these, so we must close them or leak the fds each run.
	senderUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer senderUDP.Close()
	recvUDP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer recvUDP.Close()

	senderTR := NewTransport(senderUDP)
	defer senderTR.Close()
	recvTR := NewTransport(recvUDP)
	defer recvTR.Close()

	tlsCfg, err := SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := senderTR.Listen(tlsCfg, QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	payload := make([]byte, 256*1024) // many 1-RTT packets
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	// read barrier: the sender must keep the QUIC connection open until the
	// receiver has drained the payload. Closing right after Write returns
	// (Write only buffers into quic-go) would tear the connection down
	// mid-flush and surface as "Application error 0x0 (remote)".
	readDone := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		qc, err := ln.Accept(ctx)
		if err != nil {
			errCh <- err
			return
		}
		res, err := SenderHandshake(ctx, qc, code)
		if err != nil {
			errCh <- err
			return
		}
		if _, err := res.Streams.Data.Write(payload); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
		<-readDone
		res.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	qc, err := recvTR.Dial(ctx, senderUDP.LocalAddr(), ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	res, err := ReceiverHandshake(ctx, qc, code)
	if err != nil {
		t.Fatalf("receiver handshake: %v", err)
	}
	defer res.Close()

	got := make([]byte, len(payload))
	_, readErr := io.ReadFull(res.Streams.Data, got)
	close(readDone) // release the sender to close its side
	if err := <-errCh; err != nil {
		t.Fatalf("sender side: %v", err)
	}
	if readErr != nil {
		t.Fatalf("read payload: %v", readErr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch over custom-connID connection")
	}
}
