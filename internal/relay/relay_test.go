package relay

import (
	"bytes"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pion/stun/v3"
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

// TestServer_STUNBinding verifies the relay socket answers STUN binding
// requests with the querier's reflexive address, and that non-STUN
// datagrams starting with a non-relay version byte are silently dropped.
func TestServer_STUNBinding(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := serverConn.LocalAddr().(*net.UDPAddr)

	srv := NewServer(serverConn, ServerConfig{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Run(ctx) }()

	client, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	localAddr := client.LocalAddr().(*net.UDPAddr)

	// Garbage that is neither relay frame nor STUN must not get a reply.
	if _, err := client.WriteTo([]byte{0x00, 0xde, 0xad}, relayAddr); err != nil {
		t.Fatal(err)
	}

	req, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.WriteTo(req.Raw, relayAddr); err != nil {
		t.Fatal(err)
	}

	buf := make([]byte, 1500)
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := client.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no STUN response: %v", err)
	}
	resp := &stun.Message{Raw: buf[:n]}
	if err := resp.Decode(); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Type != stun.BindingSuccess {
		t.Fatalf("response type: got %v, want BindingSuccess", resp.Type)
	}
	if resp.TransactionID != req.TransactionID {
		t.Error("transaction ID mismatch")
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(resp); err != nil {
		t.Fatalf("XOR-MAPPED-ADDRESS: %v", err)
	}
	if !mapped.IP.Equal(localAddr.IP) || mapped.Port != localAddr.Port {
		t.Errorf("mapped address: got %v:%d, want %v", mapped.IP, mapped.Port, localAddr)
	}

	cancel()
	_ = serverConn.Close()
	<-srvErr
}

// TestForwardingDisabled verifies stun-only mode: the relay still answers
// STUN binding requests but never forwards data datagrams.
func TestForwardingDisabled(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := serverConn.LocalAddr().(*net.UDPAddr)

	srv := NewServer(serverConn, ServerConfig{DisableForwarding: true})
	if srv.Forwarding() {
		t.Fatal("Forwarding() should be false when DisableForwarding is set")
	}
	tok, err := srv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Run(ctx) }()

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

	// Both peers register, then A sends a payload that must NOT reach B.
	if _, err := connA.WriteTo([]byte("hello"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := connB.WriteTo([]byte("register"), nil); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := connA.WriteTo([]byte("real message"), nil); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	_ = connB.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if n, _, err := connB.ReadFrom(buf); err == nil {
		t.Fatalf("relay forwarded %q with forwarding disabled", buf[:n])
	}

	// STUN must still answer in stun-only mode.
	stunClient, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer stunClient.Close()
	req, err := stun.Build(stun.TransactionID, stun.BindingRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stunClient.WriteTo(req.Raw, relayAddr); err != nil {
		t.Fatal(err)
	}
	_ = stunClient.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := stunClient.ReadFrom(buf)
	if err != nil {
		t.Fatalf("no STUN response in stun-only mode: %v", err)
	}
	resp := &stun.Message{Raw: buf[:n]}
	if err := resp.Decode(); err != nil {
		t.Fatalf("decode STUN response: %v", err)
	}
	if resp.Type != stun.BindingSuccess {
		t.Fatalf("STUN type: got %v, want BindingSuccess", resp.Type)
	}

	cancel()
	_ = serverConn.Close()
	<-srvErr
}

func TestForwardingEnabledByDefault(t *testing.T) {
	if !NewServer(nil, ServerConfig{}).Forwarding() {
		t.Fatal("a default relay should forward")
	}
}

// TestMetricsCounters checks that a forwarded datagram bumps bytes_forwarded
// and peak_transfer_bytes, and that exceeding the cap bumps transfers_capped.
func TestMetricsCounters(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := serverConn.LocalAddr().(*net.UDPAddr)
	srv := NewServer(serverConn, ServerConfig{MaxBytesPerSession: 10 * 1024 * 1024})
	tok, err := srv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Run(ctx) }()

	clientA, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer clientA.Close()
	clientB, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer clientB.Close()
	connA := NewClient(clientA, relayAddr, tok)
	connB := NewClient(clientB, relayAddr, tok)

	_, _ = connA.WriteTo([]byte("hello"), nil)
	_, _ = connB.WriteTo([]byte("register"), nil)
	time.Sleep(50 * time.Millisecond)
	_, _ = connA.WriteTo([]byte("a real payload"), nil)
	buf := make([]byte, 1500)
	_ = connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := connB.ReadFrom(buf); err != nil {
		t.Fatalf("B did not receive the forwarded payload: %v", err)
	}

	m := srv.Metrics()
	if m.BytesForwardedTotal == 0 {
		t.Error("bytes_forwarded_total should be > 0 after a forward")
	}
	if m.PeakTransferBytes == 0 {
		t.Error("peak_transfer_bytes should be > 0 after a forward")
	}
	if m.TransfersCappedTotal != 0 {
		t.Errorf("transfers_capped_total = %d, want 0", m.TransfersCappedTotal)
	}
	if !m.Forwarding {
		t.Error("forwarding should be true")
	}

	cancel()
	_ = serverConn.Close()
	<-srvErr
}

func TestMetricsCapHit(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := serverConn.LocalAddr().(*net.UDPAddr)
	srv := NewServer(serverConn, ServerConfig{MaxBytesPerSession: 1}) // any forwarded datagram exceeds it
	tok, _ := srv.Allocate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Run(ctx) }()

	clientA, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer clientA.Close()
	clientB, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer clientB.Close()
	connA := NewClient(clientA, relayAddr, tok)
	connB := NewClient(clientB, relayAddr, tok)

	_, _ = connA.WriteTo([]byte("x"), nil) // registers A as peerA
	_, _ = connB.WriteTo([]byte("y"), nil) // registers B; first forwarded datagram trips the 1-byte cap

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if srv.Metrics().TransfersCappedTotal > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := srv.Metrics().TransfersCappedTotal; got != 1 {
		t.Errorf("transfers_capped_total = %d, want 1", got)
	}

	cancel()
	_ = serverConn.Close()
	<-srvErr
}

// TestMaxBytes_ZeroMeansUnlimited locks in the operator escape hatch: a
// 0 byte cap forwards without ever tripping cap_hit. The hot path guards
// with `MaxBytesPerSession > 0`, so this guards against Default() (or a
// future refactor) re-mapping 0 back to a finite limit.
func TestMaxBytes_ZeroMeansUnlimited(t *testing.T) {
	serverConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	relayAddr := serverConn.LocalAddr().(*net.UDPAddr)
	srv := NewServer(serverConn, ServerConfig{MaxBytesPerSession: 0}) // unlimited
	tok, _ := srv.Allocate()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srvErr := make(chan error, 1)
	go func() { srvErr <- srv.Run(ctx) }()

	clientA, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer clientA.Close()
	clientB, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	defer clientB.Close()
	connA := NewClient(clientA, relayAddr, tok)
	connB := NewClient(clientB, relayAddr, tok)

	_, _ = connA.WriteTo([]byte("hello"), nil)
	_, _ = connB.WriteTo([]byte("register"), nil)
	time.Sleep(50 * time.Millisecond)
	_, _ = connA.WriteTo([]byte("a real payload"), nil)
	buf := make([]byte, 1500)
	_ = connB.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := connB.ReadFrom(buf); err != nil {
		t.Fatalf("B did not receive the forwarded payload: %v", err)
	}
	if got := srv.Metrics().TransfersCappedTotal; got != 0 {
		t.Errorf("transfers_capped_total = %d, want 0 (cap disabled)", got)
	}

	cancel()
	_ = serverConn.Close()
	<-srvErr
}

func TestAllowSTUNResponse_RateCap(t *testing.T) {
	s := &Server{}
	base := time.Unix(1700000000, 0)

	// The whole per-second budget is allowed.
	for i := 0; i < stunResponsesPerSec; i++ {
		if !s.allowSTUNResponse(base) {
			t.Fatalf("response %d denied within budget", i)
		}
	}
	// One more in the same window is denied.
	if s.allowSTUNResponse(base) {
		t.Fatal("expected denial after budget exhausted")
	}
	// A new one-second window resets the budget.
	if !s.allowSTUNResponse(base.Add(time.Second)) {
		t.Fatal("expected allow after window rollover")
	}
}

func TestServer_DrainRejectsNewAllocations(t *testing.T) {
	srv := NewServer(nil, ServerConfig{})
	if _, err := srv.Allocate(); err != nil {
		t.Fatalf("allocate before drain: %v", err)
	}
	srv.Drain()
	if _, err := srv.Allocate(); err == nil {
		t.Fatal("Allocate after Drain should be rejected")
	}
}

func TestServer_HealthyByDefault(t *testing.T) {
	if !NewServer(nil, ServerConfig{}).Healthy() {
		t.Fatal("a fresh relay should report healthy")
	}
}

func TestServer_ActiveAllocationsCountsLiveSessions(t *testing.T) {
	srv := NewServer(nil, ServerConfig{})
	if n := srv.ActiveAllocations(); n != 0 {
		t.Fatalf("active = %d, want 0", n)
	}
	if _, err := srv.Allocate(); err != nil {
		t.Fatal(err)
	}
	// A freshly-allocated session has lastActivity = now, so it counts as
	// in-flight for drain purposes.
	if n := srv.ActiveAllocations(); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}
}
