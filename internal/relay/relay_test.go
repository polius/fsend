package relay

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

func TestFrame_RoundtripParse(t *testing.T) {
	tok := Token{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
	payload := []byte("hello world")
	out := make([]byte, HeaderSize+len(payload))
	n := Frame(out, tok, payload)
	if n != HeaderSize+len(payload) {
		t.Errorf("Frame returned %d, want %d", n, HeaderSize+len(payload))
	}
	ver, gotTok, gotPayload, ok := Parse(out[:n])
	if !ok {
		t.Fatal("Parse returned ok=false")
	}
	if ver != ProtocolVersion {
		t.Errorf("version: got %x, want %x", ver, ProtocolVersion)
	}
	if gotTok != tok {
		t.Error("token mismatch")
	}
	if !bytes.Equal(gotPayload, payload) {
		t.Errorf("payload mismatch: got %q, want %q", gotPayload, payload)
	}
}

func TestParse_TooShort(t *testing.T) {
	_, _, _, ok := Parse(make([]byte, HeaderSize-1))
	if ok {
		t.Error("Parse should reject short datagram")
	}
}

func TestToken_StringRoundtrip(t *testing.T) {
	tok := Token{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10}
	s := tok.String()
	parsed, err := ParseToken(s)
	if err != nil {
		t.Fatalf("ParseToken: %v", err)
	}
	if parsed != tok {
		t.Error("token round-trip mismatch")
	}
}

// TestRelay_EndToEnd starts a relay server on a real UDP socket, then
// has two clients with matching tokens exchange payloads through it.
func TestRelay_EndToEnd(t *testing.T) {
	// Server-side UDP socket
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := serverConn.LocalAddr().(*net.UDPAddr)

	srv := NewServer(serverConn, ServerConfig{
		MaxBytesPerSession: 10 * 1024 * 1024,
		SessionIdleTimeout: 5 * time.Second,
	})
	tok, err := srv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Run(ctx) }()

	// Two client sockets.
	clientA, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer clientA.Close()
	clientB, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer clientB.Close()

	connA := NewClient(clientA, relayAddr, tok)
	connB := NewClient(clientB, relayAddr, tok)

	// A sends; receiver may take a moment because B hasn't registered yet.
	// The protocol handles this: A's first datagram registers A as peerA,
	// then A sends another after B has registered as peerB.
	if _, err := connA.WriteTo([]byte("hello"), nil); err != nil {
		t.Fatal(err)
	}
	// B sends a "register" packet so the server knows B's address.
	if _, err := connB.WriteTo([]byte("register"), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // give server time to register both
	// Now A sends a real message; B should receive it.
	if _, err := connA.WriteTo([]byte("real message"), nil); err != nil {
		t.Fatal(err)
	}

	// B reads.
	deadline := time.Now().Add(2 * time.Second)
	_ = connB.SetReadDeadline(deadline)
	var wg sync.WaitGroup
	var bGot string
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 1500)
		for {
			n, _, err := connB.ReadFrom(buf)
			if err != nil {
				return
			}
			payload := string(buf[:n])
			if payload == "real message" {
				bGot = payload
				return
			}
			// Skip the earlier "hello" that might be in flight.
		}
	}()
	wg.Wait()
	if bGot != "real message" {
		t.Errorf("B did not receive expected payload, got %q", bGot)
	}

	cancel()
	_ = serverConn.Close()
	<-srvErr
}
