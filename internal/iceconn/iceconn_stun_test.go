package iceconn_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/stun/v3"
	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/iceconn"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/wire"
)

// TestICE_WithSTUN proves the STUN leg of the connection ladder.
//
// We stand up a tiny in-process STUN responder on a loopback port — it
// only knows how to answer BindingRequest with XOR-MAPPED-ADDRESS — and
// configure iceconn to use it. Then we assert two things:
//
//  1. At least one server-reflexive ("srflx") candidate appears in the
//     gathered candidate set on the sender or receiver side. This proves
//     iceconn wires Options.STUNHost through to pion's AgentConfig.Urls
//     and that pion actually emits srflx after talking to our responder.
//  2. The full QUIC-over-ICE transfer still completes byte-perfect. This
//     proves the srflx candidate is usable end-to-end (not just gathered
//     and discarded).
//
// On loopback both peers' srflx address equals their host address, so
// the candidate pair the ICE agent picks may well be host↔host. That's
// fine — what we care about is "did STUN gathering succeed and didn't
// break anything." We do NOT artificially block host candidates to
// force srflx-only pairing; doing so would test a NAT scenario we can't
// realistically simulate inside a single process without a fake NAT
// stack, which is squarely in over-engineering territory.
func TestICE_WithSTUN(t *testing.T) {
	const fileSize = 1 * 1024 * 1024 // 1 MB — just enough to mean something

	// --- Mini STUN responder ---
	stunConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer stunConn.Close()
	stunPort := stunConn.LocalAddr().(*net.UDPAddr).Port

	stunCtx, stunCancel := context.WithCancel(context.Background())
	defer stunCancel()
	go runMiniSTUN(t, stunCtx, stunConn)

	// --- Source file ---
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

	const (
		senderUfrag   = "Suuuu"
		senderPwd     = "spppppppppppppppppppppppp"
		receiverUfrag = "Ruuuu"
		receiverPwd   = "rpppppppppppppppppppppppp"
	)

	senderAgent, err := iceconn.New(iceconn.Options{
		LocalUfrag:  senderUfrag,
		LocalPwd:    senderPwd,
		RemoteUfrag: receiverUfrag,
		RemotePwd:   receiverPwd,
		STUNHost:    "127.0.0.1",
		STUNPort:    stunPort,
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
		STUNHost:    "127.0.0.1",
		STUNPort:    stunPort,
	})
	if err != nil {
		t.Fatalf("receiver agent: %v", err)
	}
	defer receiverAgent.Close()

	// Pump candidates between agents AND watch what types appear.
	var (
		typesMu  sync.Mutex
		gathered = map[ice.CandidateType]int{}
	)
	noteCandidate := func(s string) {
		c, err := ice.UnmarshalCandidate(s)
		if err != nil {
			return
		}
		typesMu.Lock()
		gathered[c.Type()]++
		typesMu.Unlock()
	}

	var pumpWG sync.WaitGroup
	pumpWG.Add(2)
	go func() {
		defer pumpWG.Done()
		for c := range senderAgent.LocalCandidates() {
			noteCandidate(c)
			if err := receiverAgent.AddRemoteCandidate(c); err != nil {
				t.Errorf("recv add candidate: %v", err)
				return
			}
		}
	}()
	go func() {
		defer pumpWG.Done()
		for c := range receiverAgent.LocalCandidates() {
			noteCandidate(c)
			if err := senderAgent.AddRemoteCandidate(c); err != nil {
				t.Errorf("send add candidate: %v", err)
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

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

	// --- Run a real transfer through the ICE-selected pair + QUIC ---
	senderTLS, err := quicconn.SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	senderTransport := &quic.Transport{Conn: sRes.conn}
	defer senderTransport.Close()
	senderLn, err := senderTransport.Listen(senderTLS, quicconn.QuicConfig())
	if err != nil {
		t.Fatal(err)
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
		recvTransport := &quic.Transport{Conn: rRes.conn}
		defer recvTransport.Close()
		dialAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1}
		c, err := recvTransport.Dial(ctx, dialAddr, quicconn.ReceiverTLSConfig(), quicconn.QuicConfig())
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
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("destination bytes differ from source")
	}

	// The core STUN assertion: at least one srflx candidate was
	// gathered. If our mini STUN responder hadn't replied, or iceconn
	// hadn't plumbed STUNHost through, this map would only contain
	// CandidateTypeHost.
	typesMu.Lock()
	defer typesMu.Unlock()
	srflxCount := gathered[ice.CandidateTypeServerReflexive]
	if srflxCount == 0 {
		t.Errorf("no srflx candidate gathered; saw: %v "+
			"(expected at least one — STUN responder unreached or iceconn "+
			"didn't plumb Options.STUNHost into pion's AgentConfig.Urls)",
			summarizeTypes(gathered))
	}
	t.Logf("STUN OK: gathered %d srflx + %d host across both peers",
		srflxCount, gathered[ice.CandidateTypeHost])
}

func summarizeTypes(m map[ice.CandidateType]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k.String()] = v
	}
	return out
}

// runMiniSTUN answers RFC 5389 BindingRequest with BindingSuccess +
// XOR-MAPPED-ADDRESS = the requester's source address. That's the
// entire STUN-server contract pion's ICE agent depends on for srflx
// candidate gathering. We deliberately don't implement anything else —
// no TURN, no authentication, no fingerprint validation on requests.
func runMiniSTUN(t *testing.T, ctx context.Context, conn *net.UDPConn) {
	buf := make([]byte, 1500)
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		n, addr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			return // socket closed
		}
		req := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
		if err := req.Decode(); err != nil {
			continue
		}
		if req.Type != stun.BindingRequest {
			continue
		}
		resp, err := stun.Build(
			stun.NewTransactionIDSetter(req.TransactionID),
			stun.BindingSuccess,
			&stun.XORMappedAddress{IP: addr.IP, Port: addr.Port},
			stun.Fingerprint,
		)
		if err != nil {
			t.Logf("stun build: %v", err)
			continue
		}
		_, _ = conn.WriteToUDP(resp.Raw, addr)
	}
}
