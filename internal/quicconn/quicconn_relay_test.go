package quicconn

import (
	"bytes"
	"context"
	"crypto/rand"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/wire"
)

// TestQUIC_OverRelay drives the headline integration test for fsend's
// cross-internet path:
//
//  1. Start an in-process UDP relay (internal/relay.Server).
//  2. Allocate a token.
//  3. Both peers open their own UDP sockets and wrap them with relay.Conn.
//  4. Hand each peer's relay.Conn to a quic-go Transport.
//  5. Sender opens a QUIC listener via Transport.Listen.
//  6. Receiver dials via Transport.Dial.
//  7. internal/transfer runs the full file transfer through them.
//  8. Verify byte-perfect.
//
// This proves end-to-end:
//
//	real bytes → relay-framed → relay-server demux → relay-framed back →
//	peer-side unframe → quic-go reads as a UDP datagram → QUIC handshake →
//	TLS 1.3 → fsend wire protocol → file written to disk → BLAKE3 root match.
func TestQUIC_OverRelay(t *testing.T) {
	const fileSize = 2 * 1024 * 1024 // 2 MB

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

	// --- Relay server ---
	relayConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer relayConn.Close()
	relayAddr := relayConn.LocalAddr().(*net.UDPAddr)

	relaySrv := relay.NewServer(relayConn, relay.ServerConfig{
		MaxBytesPerSession: 100 * 1024 * 1024,
	})
	tok, err := relaySrv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	relayCtx, relayCancel := context.WithCancel(context.Background())
	defer relayCancel()
	go relaySrv.Run(relayCtx)

	// --- Sender's local UDP socket, wrapped as a relay.Conn ---
	senderUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer senderUDP.Close()
	senderConn := relay.NewClient(senderUDP, relayAddr, tok)

	// --- Receiver's local UDP socket, wrapped as a relay.Conn ---
	receiverUDP, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer receiverUDP.Close()
	receiverConn := relay.NewClient(receiverUDP, relayAddr, tok)

	// Bootstrap: each peer sends one byte so the relay learns both
	// addresses. The relay drops these (PeerA registers, PeerB
	// registers — only after both, forwarding actually works).
	if _, err := senderConn.WriteTo([]byte{0}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := receiverConn.WriteTo([]byte{0}, nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)

	// --- Sender: QUIC Transport over relay.Conn ---
	senderTLS, err := SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	senderTransport := &quic.Transport{Conn: senderConn}
	defer senderTransport.Close()
	senderLn, err := senderTransport.Listen(senderTLS, QuicConfig())
	if err != nil {
		t.Fatalf("Transport.Listen: %v", err)
	}
	defer senderLn.Close()

	items, err := transfer.Walk([]string{srcPath}, nil)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var sendErr, recvErr error
	var wg sync.WaitGroup
	wg.Add(2)

	// Sender goroutine.
	go func() {
		defer wg.Done()
		c, err := senderLn.Accept(ctx)
		if err != nil {
			sendErr = err
			return
		}
		res, err := SenderHandshake(ctx, c, "abc-defg-hjk")
		if err != nil {
			sendErr = err
			return
		}
		defer res.Close()
		sendErr = transfer.Send(ctx, &res.Streams, transfer.SendOptions{
			Mode:          wire.ModeFiles,
			Sources:       items,
			Hostname:      "alice",
			OS:            "darwin",
			ClientVersion: "test",
		})
	}()

	// Receiver goroutine.
	go func() {
		defer wg.Done()
		receiverTransport := &quic.Transport{Conn: receiverConn}
		defer receiverTransport.Close()

		// Sender's address as the QUIC stack sees it is the synthetic
		// peer label — but Transport.Dial needs a real net.UDPAddr it
		// can sendto. The relay.Conn.WriteTo ignores addr, so we can
		// pass any UDPAddr; quic-go uses it as a routing tag locally.
		dialAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1} // synthetic
		c, err := receiverTransport.Dial(ctx, dialAddr, ReceiverTLSConfig(), QuicConfig())
		if err != nil {
			recvErr = err
			return
		}
		res, err := ReceiverHandshake(ctx, c, "abc-defg-hjk")
		if err != nil {
			recvErr = err
			return
		}
		defer res.Close()
		recvErr = transfer.Recv(ctx, &res.Streams, transfer.RecvOptions{
			Hostname:      "bob",
			OS:            "linux",
			ClientVersion: "test",
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
		t.Errorf("destination bytes differ from source")
	}
}
