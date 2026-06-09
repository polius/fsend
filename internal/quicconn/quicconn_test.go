package quicconn

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/wire"
)

// TestNegotiatesPostQuantum locks in the headline property: two fsend
// peers must land on the X25519MLKEM768 hybrid, not plain X25519. Asserts
// the wire result so a silent downgrade fails CI.
func TestNegotiatesPostQuantum(t *testing.T) {
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-jkm")
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	defer ln.Close()

	type acceptResult struct {
		res *AcceptResult
		err error
	}
	// Keep both ends alive until both curves are read — closing the
	// sender as soon as it captures the curve would race the receiver's
	// data-stream prime read.
	acceptCh := make(chan acceptResult, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		res, err := ln.Accept(ctx)
		acceptCh <- acceptResult{res, err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dialRes, err := Dial(ctx, ln.ln.Addr().String(), "abc-defg-jkm")
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer dialRes.Close()

	acc := <-acceptCh
	if acc.err != nil {
		t.Fatalf("Accept: %v", acc.err)
	}
	defer acc.res.Close()

	for _, got := range []tls.CurveID{
		acc.res.Conn.ConnectionState().TLS.CurveID,
		dialRes.Conn.ConnectionState().TLS.CurveID,
	} {
		if got != tls.X25519MLKEM768 {
			t.Errorf("negotiated curve = %v, want X25519MLKEM768 (post-quantum downgrade!)", got)
		}
	}
}

// TestQUIC_LoopbackTransfer is the load-bearing integration test for the
// transport: send a 4 MB random file from one in-process goroutine to
// another over real QUIC on the loopback interface. Verifies that the
// transport hands the transfer engine streams it can use end-to-end.
func TestQUIC_LoopbackTransfer(t *testing.T) {
	const fileSize = 4 * 1024 * 1024

	srcDir := t.TempDir()
	dstDir := t.TempDir()
	srcPath := filepath.Join(srcDir, "payload.bin")

	payload := make([]byte, fileSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	// Sender side: start QUIC listener on a free port on loopback.
	ln, err := ListenAddr("127.0.0.1:0", "abc-defg-hjk")
	if err != nil {
		t.Fatalf("ListenAddr: %v", err)
	}
	defer ln.Close()
	listenAddr := ln.ln.Addr().String()

	items, err := transfer.Walk([]string{srcPath})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		res, err := ln.Accept(ctx)
		if err != nil {
			sendErr = err
			return
		}
		defer res.Close()
		sendErr = transfer.Send(ctx, &res.Streams, transfer.SendOptions{
			Items:         items,
			Hostname:      "alice",
			OS:            "darwin",
			ClientVersion: "0.1.0-test",
			TransferKind:  wire.TransferSingleFile,
		})
	}()

	go func() {
		defer wg.Done()
		// Tiny pause so the listener is definitely ready. Belt-and-suspenders;
		// ListenAddr returns only after the socket is up.
		time.Sleep(50 * time.Millisecond)
		res, err := Dial(ctx, listenAddr, "abc-defg-hjk")
		if err != nil {
			recvErr = err
			return
		}
		defer res.Close()
		recvErr = transfer.Recv(ctx, &res.Streams, transfer.RecvOptions{
			Hostname:      "bob",
			OS:            "linux",
			ClientVersion: "0.1.0-test",
			TargetDir:     dstDir,
		})
	}()

	wg.Wait()

	if sendErr != nil {
		t.Errorf("send: %v", sendErr)
	}
	if recvErr != nil {
		t.Errorf("recv: %v", recvErr)
	}

	got, err := os.ReadFile(filepath.Join(dstDir, "payload.bin"))
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("destination bytes differ from source (src=%d, dst=%d)", len(payload), len(got))
	}
}
