package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/relay"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
)

// relayFixture is a pairing server whose advertised relay UDP port is a
// black hole — what a network that blocks UDP looks like from the client —
// while the real relay socket stays reachable for a peer that has UDP.
type relayFixture struct {
	client   *signaling.Client
	realUDP  string // the relay's actual UDP address
	sender   *signaling.CreateResult
	receiver *server.JoinSessionResponse
}

func newRelayFixture(t *testing.T, withWS bool) *relayFixture {
	t.Helper()
	old := relayUDPWait
	relayUDPWait = 200 * time.Millisecond
	t.Cleanup(func() { relayUDPWait = old })

	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	blackhole, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rs := relay.NewServer(udp, relay.ServerConfig{Logger: logger})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = rs.Run(ctx); close(done) }()

	var alloc server.RelayAllocator = rs
	if !withWS {
		alloc = udpOnlyRelay{rs}
	}
	srv := server.New(server.Config{Logger: logger}).WithRelay(alloc, blackhole.LocalAddr().(*net.UDPAddr).Port)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		cancel()
		_ = udp.Close()
		_ = blackhole.Close()
		<-done
	})

	client := signaling.New(ts.URL, "test")
	created, err := client.Create(context.Background(), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	joined, err := client.Join(context.Background(), created.Code)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	return &relayFixture{client: client, realUDP: udp.LocalAddr().String(), sender: created, receiver: joined}
}

// udpOnlyRelay hides the relay's WebSocket side, like a server that predates it.
type udpOnlyRelay struct{ server.RelayAllocator }

func (f *relayFixture) dial(t *testing.T, sessionID, roleToken string, realUDP bool) (net.PacketConn, connpath.Info, error) {
	t.Helper()
	alloc, err := f.client.AllocateRelay(context.Background(), sessionID, roleToken)
	if err != nil {
		t.Fatalf("AllocateRelay: %v", err)
	}
	if realUDP {
		alloc.RelayAddr = f.realUDP
	}
	return dialRelay(context.Background(), f.client, alloc, sessionID, roleToken)
}

// TestDialRelay_UDPBlockedFallsBackToTCP: with UDP to the relay blocked,
// both peers end up on the WebSocket fallback — or one on it and one on
// UDP — and a QUIC transfer still completes.
func TestDialRelay_UDPBlockedFallsBackToTCP(t *testing.T) {
	for _, tc := range []struct {
		name         string
		senderHasUDP bool
	}{
		{"both_tcp", false},
		{"udp_to_tcp", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayFixture(t, true)
			spc, spath, err := f.dial(t, f.sender.SessionID, f.sender.RoleToken, tc.senderHasUDP)
			if err != nil {
				t.Fatalf("sender dialRelay: %v", err)
			}
			defer spc.Close()
			rpc, rpath, err := f.dial(t, f.receiver.SessionID, f.receiver.RoleToken, false)
			if err != nil {
				t.Fatalf("receiver dialRelay: %v", err)
			}
			defer rpc.Close()
			if !strings.HasSuffix(rpath.RelayAddr, "over TCP") {
				t.Errorf("receiver path = %q, want the TCP fallback", rpath.RelayAddr)
			}
			if tc.senderHasUDP == strings.HasSuffix(spath.RelayAddr, "over TCP") {
				t.Errorf("sender path = %q (has UDP: %v)", spath.RelayAddr, tc.senderHasUDP)
			}
			quicTransfer(t, spc, rpc, 4<<20)
		})
	}
}

func TestDialRelay_UDPBlockedNoFallbackIsE042(t *testing.T) {
	f := newRelayFixture(t, false)
	_, _, err := f.dial(t, f.sender.SessionID, f.sender.RoleToken, false)
	if !errors.Is(err, fserrors.ErrUDPBlocked) {
		t.Fatalf("err = %v, want ErrUDPBlocked", err)
	}
	if entry, _ := fserrors.Lookup(err); entry.Code != "E042" {
		t.Errorf("code = %s, want E042", entry.Code)
	}
}

// quicTransfer runs QUIC over the two relay conns and moves n random bytes
// from receiver to sender (the dialing side opens the stream).
func quicTransfer(t *testing.T, listenPC, dialPC net.PacketConn, n int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tlsCfg, err := quicconn.SenderTLSConfig()
	if err != nil {
		t.Fatal(err)
	}
	ltr := quicconn.NewTransport(listenPC)
	defer ltr.Close()
	ln, err := ltr.Listen(tlsCfg, quicconn.QuicConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	dtr := quicconn.NewTransport(dialPC)
	defer dtr.Close()

	payload := make([]byte, n)
	_, _ = rand.Read(payload)
	sent := make(chan error, 1)
	go func() {
		qc, err := dtr.Dial(ctx, relay.SyntheticPeer, quicconn.ReceiverTLSConfig(), quicconn.QuicConfig())
		if err != nil {
			sent <- err
			return
		}
		s, err := qc.OpenStreamSync(ctx)
		if err != nil {
			sent <- err
			return
		}
		if _, err := s.Write(payload); err != nil {
			sent <- err
			return
		}
		sent <- s.Close()
		<-ctx.Done() // keep the connection up until the reader is done
	}()
	qc, err := ln.Accept(ctx)
	if err != nil {
		t.Fatalf("QUIC accept: %v", err)
	}
	s, err := qc.AcceptStream(ctx)
	if err != nil {
		t.Fatalf("accept stream: %v", err)
	}
	got, err := io.ReadAll(s)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-sent; err != nil {
		t.Fatalf("send: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: got %d bytes, want %d", len(got), len(payload))
	}
}
