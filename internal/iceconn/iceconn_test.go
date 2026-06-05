package iceconn_test

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

	"github.com/polius/fsend/internal/iceconn"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/wire"
)

// TestICE_LoopbackThenQUIC drives the headline integration test for
// fsend's direct-P2P path:
//
//  1. Stand up two ICE agents (sender = controlling, receiver = controlled).
//  2. Exchange their gathered candidates over a goroutine-only "signaling
//     bus" — no real server required since we just need both sides to
//     learn each other's candidates.
//  3. Call Dial (sender) and Accept (receiver) — pion picks a candidate
//     pair and gives us each side a net.PacketConn.
//  4. Hand each PacketConn to a quic.Transport.
//  5. Run the full fsend transfer protocol over QUIC-over-ICE.
//  6. Verify byte-perfect file delivery.
//
// On a loopback host there is no NAT to traverse; pion just picks the
// 127.0.0.1 host candidate. That's still the right thing to test —
// real-world NAT punching happens transparently inside pion, and the
// surface this test exercises (gathering, candidate exchange, the
// PacketConn adapter, the QUIC handoff) is what fsend owns.
func TestICE_LoopbackThenQUIC(t *testing.T) {
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

	// ICE credentials (in production these come from the signaling
	// server's CreateSession/JoinSession responses).
	const (
		senderUfrag   = "Suuuu"
		senderPwd     = "spppppppppppppppppppppppp"
		receiverUfrag = "Ruuuu"
		receiverPwd   = "rpppppppppppppppppppppppp"
	)

	// STUNHost empty → host candidates only (loopback test, no real STUN).
	senderAgent, err := iceconn.New(iceconn.Options{
		LocalUfrag:  senderUfrag,
		LocalPwd:    senderPwd,
		RemoteUfrag: receiverUfrag,
		RemotePwd:   receiverPwd,
	})
	if err != nil {
		t.Fatalf("sender agent: %v", err)
	}
	defer senderAgent.Close()

	receiverAgent, err := iceconn.New(iceconn.Options{
		LocalUfrag:  receiverUfrag,
		LocalPwd:    receiverPwd,
		RemoteUfrag: senderUfrag,
		RemotePwd:   senderPwd,
	})
	if err != nil {
		t.Fatalf("receiver agent: %v", err)
	}
	defer receiverAgent.Close()

	// Candidate-bus: drain each agent's local candidates and feed them
	// into the other side. Mirrors what runSend/runReceive will do via
	// signaling.Client.{Push,Pull}Candidates in production.
	var pumpWG sync.WaitGroup
	pumpWG.Add(2)
	go func() {
		defer pumpWG.Done()
		for c := range senderAgent.LocalCandidates() {
			if err := receiverAgent.AddRemoteCandidate(c); err != nil {
				t.Errorf("recv side add candidate: %v", err)
				return
			}
		}
	}()
	go func() {
		defer pumpWG.Done()
		for c := range receiverAgent.LocalCandidates() {
			if err := senderAgent.AddRemoteCandidate(c); err != nil {
				t.Errorf("send side add candidate: %v", err)
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Drive Dial / Accept in parallel — they only return once the ICE
	// connection check picks a working pair.
	type icePair struct {
		conn net.PacketConn
		err  error
	}
	senderRes := make(chan icePair, 1)
	receiverRes := make(chan icePair, 1)
	go func() {
		c, err := senderAgent.Dial(ctx)
		senderRes <- icePair{c, err}
	}()
	go func() {
		c, err := receiverAgent.Accept(ctx)
		receiverRes <- icePair{c, err}
	}()

	sRes := <-senderRes
	rRes := <-receiverRes
	if sRes.err != nil {
		t.Fatalf("sender ICE dial: %v", sRes.err)
	}
	if rRes.err != nil {
		t.Fatalf("receiver ICE accept: %v", rRes.err)
	}
	defer sRes.conn.Close()
	defer rRes.conn.Close()

	// Hand both PacketConns to quic-go Transports and run the full
	// fsend transfer flow. This is exactly what cmd/fsend will do once
	// ICE is wired into the strategy ladder.
	senderTLS, err := quicconn.SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	senderTransport := &quic.Transport{Conn: sRes.conn}
	defer senderTransport.Close()
	senderLn, err := senderTransport.Listen(senderTLS, quicconn.QuicConfig())
	if err != nil {
		t.Fatalf("transport listen: %v", err)
	}
	defer senderLn.Close()

	items, err := transfer.Walk([]string{srcPath})
	if err != nil {
		t.Fatal(err)
	}

	var sendErr, recvErr error
	var transferWG sync.WaitGroup
	transferWG.Add(2)

	go func() {
		defer transferWG.Done()
		c, err := senderLn.Accept(ctx)
		if err != nil {
			sendErr = err
			return
		}
		res, err := quicconn.SenderHandshake(ctx, c, "abc-defg-hjk")
		if err != nil {
			sendErr = err
			return
		}
		defer res.Close()
		sendErr = transfer.Send(ctx, &res.Streams, transfer.SendOptions{
			Items:         items,
			Hostname:      "alice",
			OS:            "darwin",
			ClientVersion: "test",
			TransferKind:  wire.TransferSingleFile,
		})
	}()

	go func() {
		defer transferWG.Done()
		receiverTransport := &quic.Transport{Conn: rRes.conn}
		defer receiverTransport.Close()

		// Like the relay test: the actual route through ice.Conn ignores
		// the dial address; quic-go uses it as a per-peer routing tag.
		dialAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1}
		c, err := receiverTransport.Dial(ctx, dialAddr, quicconn.ReceiverTLSConfig(), quicconn.QuicConfig())
		if err != nil {
			recvErr = err
			return
		}
		res, err := quicconn.ReceiverHandshake(ctx, c, "abc-defg-hjk")
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

	transferWG.Wait()
	pumpWG.Wait()

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
