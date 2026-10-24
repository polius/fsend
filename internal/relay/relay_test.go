package relay

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v4"
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

func TestControl_RoundtripParse(t *testing.T) {
	tok, id := Token{1, 2, 3}, PeerID{9, 8, 7}
	c := [cookieSize]byte{0xAA, 0xBB}
	gotTok, typ, gotID, gotCookie, ok := parseControl(frameControl(tok, msgRegister, id, &c))
	if !ok || gotTok != tok || typ != msgRegister || gotID != id || gotCookie != c {
		t.Fatalf("control roundtrip mismatch: ok=%v tok=%v typ=%d id=%v", ok, gotTok, typ, gotID)
	}
	if _, _, _, _, ok := parseControl(make([]byte, ControlSize-1)); ok {
		t.Error("parseControl should reject a short frame")
	}
}

func TestPeerIDFor_StableAndDistinct(t *testing.T) {
	if PeerIDFor("a") != PeerIDFor("a") {
		t.Error("PeerIDFor must be deterministic")
	}
	if PeerIDFor("a") == PeerIDFor("b") {
		t.Error("PeerIDFor must separate secrets")
	}
}

// startRelay runs a relay on a loopback UDP socket for the test's lifetime.
func startRelay(t *testing.T, cfg ServerConfig) (*Server, *net.UDPAddr) {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(conn, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		_ = conn.Close()
		<-done
	})
	return srv, conn.LocalAddr().(*net.UDPAddr)
}

func listenLoopback(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// registered returns a UDP client that completed the registration handshake.
func registered(t *testing.T, relayAddr *net.UDPAddr, tok Token) *Conn {
	t.Helper()
	c := NewClient(listenLoopback(t), relayAddr, tok)
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.Register(ctx); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return c
}

// expectPayload reads from c until want arrives or the deadline passes.
func expectPayload(t *testing.T, c net.PacketConn, want []byte) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, MaxDatagram)
	for {
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			t.Fatalf("did not receive %d-byte payload: %v", len(want), err)
		}
		if bytes.Equal(buf[:n], want) {
			return
		}
	}
}

// TestRelay_EndToEnd has two registered clients exchange payloads.
func TestRelay_EndToEnd(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{MaxBytesPerSession: 10 * 1024 * 1024})
	tok, err := srv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	a, b := registered(t, relayAddr, tok), registered(t, relayAddr, tok)
	if _, err := a.WriteTo([]byte("a→b"), nil); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, b, []byte("a→b"))
	if _, err := b.WriteTo([]byte("b→a"), nil); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, a, []byte("b→a"))
}

// TestRegister_SpoofedSourceCannotRegister locks in the return-routability
// check: neither a bare data frame nor a cookie lifted from another address
// registers a source, so a spoofed packet can't aim the relay at a victim.
func TestRegister_SpoofedSourceCannotRegister(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{})
	tok, _ := srv.Allocate()
	victim, attacker := listenLoopback(t), listenLoopback(t)

	// The pre-cookie attack: a first data frame used to register its source.
	frame := make([]byte, HeaderSize+64)
	Frame(frame, tok, make([]byte, 64))
	_, _ = victim.WriteTo(frame, relayAddr)

	// The victim's HELLO yields a cookie bound to the victim's address...
	id := PeerID{1}
	_, _ = victim.WriteTo(frameControl(tok, msgHello, id, nil), relayAddr)
	cookie := readControl(t, victim, msgChallenge)

	// ...which is worthless from any other address.
	_, _ = attacker.WriteTo(frameControl(tok, msgRegister, id, &cookie), relayAddr)
	_ = attacker.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if n, _, err := attacker.ReadFrom(make([]byte, MaxDatagram)); err == nil {
		t.Fatalf("attacker got a %d-byte reply to a stolen cookie", n)
	}
	a := srv.lookup(tok)
	if a.peers[0].Load() != nil || a.peers[1].Load() != nil {
		t.Fatal("a source registered without echoing its own cookie")
	}

	// The real owner of the cookie registers.
	_, _ = victim.WriteTo(frameControl(tok, msgRegister, id, &cookie), relayAddr)
	readControl(t, victim, msgRegistered)
	if p := a.peers[0].Load(); p == nil || !udpEqual(p.ep.udp, victim.LocalAddr().(*net.UDPAddr)) {
		t.Fatal("cookie owner not registered")
	}
}

// readControl reads until a control frame of typ arrives and returns its cookie.
func readControl(t *testing.T, c *net.UDPConn, typ byte) [cookieSize]byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	buf := make([]byte, MaxDatagram)
	for {
		n, _, err := c.ReadFrom(buf)
		if err != nil {
			t.Fatalf("no control frame %d: %v", typ, err)
		}
		if _, got, _, cookie, ok := parseControl(buf[:n]); ok && got == typ {
			return cookie
		}
	}
}

// TestRebinding_RelearnsAddress: a peer whose address changes mid-session
// (NAT rebinding, network switch) is challenged on its first datagram from
// the new address and re-registers there, keeping its slot.
func TestRebinding_RelearnsAddress(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{})
	tok, _ := srv.Allocate()
	idA := PeerID{0xA}
	a := NewClient(listenLoopback(t), relayAddr, tok).WithPeerID(idA)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Register(ctx); err != nil {
		t.Fatal(err)
	}
	b := registered(t, relayAddr, tok)

	// A "moves": same peer id, new socket, no explicit Register.
	_ = a.Close()
	moved := NewClient(listenLoopback(t), relayAddr, tok).WithPeerID(idA)
	defer moved.Close()
	got := make(chan []byte, 8)
	go func() { // ReadFrom answers the relay's challenge
		buf := make([]byte, MaxDatagram)
		for {
			n, _, err := moved.ReadFrom(buf)
			if err != nil {
				return
			}
			got <- append([]byte(nil), buf[:n]...)
		}
	}()

	payload := bytes.Repeat([]byte{'m'}, 64) // ≥ ControlSize, so it earns a challenge
	buf := make([]byte, MaxDatagram)
	for i := 0; ; i++ {
		if i > 20 {
			t.Fatal("relay never re-learned the moved peer")
		}
		_, _ = moved.WriteTo(payload, nil)
		_ = b.SetReadDeadline(time.Now().Add(150 * time.Millisecond))
		if n, _, err := b.ReadFrom(buf); err == nil && bytes.Equal(buf[:n], payload) {
			break
		}
	}
	_ = b.SetReadDeadline(time.Time{})
	_, _ = b.WriteTo([]byte("back"), nil)
	select {
	case m := <-got:
		if string(m) != "back" {
			t.Fatalf("moved peer got %q", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reply never reached the moved peer")
	}

	// A third peer can't take a slot: the allocation is 1↔1.
	third := NewClient(listenLoopback(t), relayAddr, tok)
	defer third.Close()
	ctx3, cancel3 := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel3()
	if err := third.Register(ctx3); !errors.Is(err, ErrNoReply) {
		t.Fatalf("third peer Register = %v, want ErrNoReply", err)
	}
}

// TestOversizeDatagrams: a datagram over 1500 bytes is forwarded whole; one
// over MaxDatagram is dropped and counted, never truncated.
func TestOversizeDatagrams(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{})
	tok, _ := srv.Allocate()
	a, b := registered(t, relayAddr, tok), registered(t, relayAddr, tok)

	big := bytes.Repeat([]byte{'x'}, 1700)
	if _, err := a.WriteTo(big, nil); err != nil {
		t.Fatal(err)
	}
	expectPayload(t, b, big)

	huge := make([]byte, 3000)
	Frame(huge, tok, huge[HeaderSize:])
	if _, err := a.underlying.WriteTo(huge, relayAddr); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for srv.Metrics().DatagramsOversizeTotal == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := srv.Metrics().DatagramsOversizeTotal; got != 1 {
		t.Fatalf("datagrams_oversize_total = %d, want 1", got)
	}
}

// TestServer_STUNBinding verifies the relay socket answers STUN binding
// requests with the querier's reflexive address, and that non-STUN
// datagrams starting with a non-relay version byte are silently dropped.
func TestServer_STUNBinding(t *testing.T) {
	_, relayAddr := startRelay(t, ServerConfig{})
	client := listenLoopback(t)
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
}

func TestProbeSTUN(t *testing.T) {
	_, relayAddr := startRelay(t, ServerConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := ProbeSTUN(ctx, relayAddr.String())
	if err != nil {
		t.Fatalf("ProbeSTUN: %v", err)
	}
	if !got.IP.IsLoopback() {
		t.Errorf("mapped = %v, want a loopback address", got)
	}

	// A silent UDP port is how a blocked network looks.
	silent := listenLoopback(t)
	ctx2, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	if _, err := ProbeSTUN(ctx2, silent.LocalAddr().String()); !errors.Is(err, ErrNoReply) {
		t.Fatalf("ProbeSTUN on a silent port = %v, want ErrNoReply", err)
	}
}

// TestForwardingDisabled verifies stun-only mode: the relay still answers
// STUN binding requests but never registers peers or forwards data.
func TestForwardingDisabled(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{DisableForwarding: true})
	if srv.Forwarding() {
		t.Fatal("Forwarding() should be false when DisableForwarding is set")
	}
	tok, err := srv.Allocate()
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(listenLoopback(t), relayAddr, tok)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := c.Register(ctx); !errors.Is(err, ErrNoReply) {
		t.Fatalf("Register with forwarding disabled = %v, want ErrNoReply", err)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	if _, err := ProbeSTUN(ctx2, relayAddr.String()); err != nil {
		t.Fatalf("no STUN response in stun-only mode: %v", err)
	}
}

func TestForwardingEnabledByDefault(t *testing.T) {
	if !NewServer(nil, ServerConfig{}).Forwarding() {
		t.Fatal("a default relay should forward")
	}
}

// TestMetricsCounters checks that a forwarded datagram bumps bytes_forwarded
// and peak_transfer_bytes, and that exceeding the cap bumps transfers_capped.
func TestMetricsCounters(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{MaxBytesPerSession: 10 * 1024 * 1024})
	tok, _ := srv.Allocate()
	a, b := registered(t, relayAddr, tok), registered(t, relayAddr, tok)
	_, _ = a.WriteTo([]byte("a real payload"), nil)
	expectPayload(t, b, []byte("a real payload"))

	m := srv.Metrics()
	if m.BytesForwardedTotal == 0 {
		t.Error("bytes_forwarded_total should be > 0 after a forward")
	}
	if m.PeakTransferBytes == 0 {
		t.Error("peak_transfer_bytes should be > 0 after a forward")
	}
	if m.TransfersTotal != 1 {
		t.Errorf("transfers_total = %d, want 1 after one transfer forwarded data", m.TransfersTotal)
	}
	if m.TransfersCappedTotal != 0 {
		t.Errorf("transfers_capped_total = %d, want 0", m.TransfersCappedTotal)
	}
	if !m.Forwarding {
		t.Error("forwarding should be true")
	}
}

// TestMetricsTransfersTotal_OnlyCountsForwarded locks in the metric's
// meaning: a slot allocated for STUN but that never forwards a byte (direct
// path won, or only one peer showed up) must not inflate transfers_total.
func TestMetricsTransfersTotal_OnlyCountsForwarded(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{})
	tok, _ := srv.Allocate()
	if got := srv.Metrics().TransfersTotal; got != 0 {
		t.Fatalf("transfers_total = %d after allocate alone, want 0", got)
	}
	// Only one peer registers and sends: nothing is forwarded.
	a := registered(t, relayAddr, tok)
	_, _ = a.WriteTo([]byte("hello"), nil)
	time.Sleep(100 * time.Millisecond)
	if got := srv.Metrics().TransfersTotal; got != 0 {
		t.Errorf("transfers_total = %d with no data forwarded, want 0", got)
	}
}

// waitFor polls cond for up to 2 s.
func waitFor(cond func() bool) {
	deadline := time.Now().Add(2 * time.Second)
	for !cond() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

func TestMetricsCapHit(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{MaxBytesPerSession: 1}) // any forwarded datagram exceeds it
	tok, _ := srv.Allocate()
	a, _ := registered(t, relayAddr, tok), registered(t, relayAddr, tok)
	_, _ = a.WriteTo([]byte("x"), nil)
	waitFor(func() bool { return srv.Metrics().TransfersCappedTotal > 0 })
	if got := srv.Metrics().TransfersCappedTotal; got != 1 {
		t.Errorf("transfers_capped_total = %d, want 1", got)
	}
}

// TestBudgetHitEvicts checks the daily budget trips the circuit breaker:
// once the day's bytes are spent the transfer is evicted with the
// daily_budget reason.
func TestBudgetHitEvicts(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{MaxBytesPerDay: 1}) // any forwarded datagram exceeds it
	tok, _ := srv.Allocate()
	a, _ := registered(t, relayAddr, tok), registered(t, relayAddr, tok)
	_, _ = a.WriteTo([]byte("x"), nil)
	waitFor(func() bool { return srv.Status(tok) == ReasonBudgetHit })
	if got := srv.Status(tok); got != ReasonBudgetHit {
		t.Errorf("Status = %q, want %q", got, ReasonBudgetHit)
	}
}

// TestAllocate_BudgetExhausted checks a new allocation is refused with the
// sentinel once the day's budget is spent, so the signaling layer can flag
// the response and clients fail fast rather than dialing a dead slot.
func TestAllocate_BudgetExhausted(t *testing.T) {
	srv := NewServer(nil, ServerConfig{MaxBytesPerDay: 10})
	srv.budget.charge(time.Now(), 10) // spend it
	if _, err := srv.Allocate(); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("Allocate err = %v, want ErrBudgetExhausted", err)
	}
}

// TestMaxBytes_ZeroMeansUnlimited locks in the operator escape hatch: a
// 0 byte cap forwards without ever tripping cap_hit. The hot path guards
// with `MaxBytesPerSession > 0`, so this guards against Default() (or a
// future refactor) re-mapping 0 back to a finite limit.
func TestMaxBytes_ZeroMeansUnlimited(t *testing.T) {
	srv, relayAddr := startRelay(t, ServerConfig{MaxBytesPerSession: 0}) // unlimited
	tok, _ := srv.Allocate()
	a, b := registered(t, relayAddr, tok), registered(t, relayAddr, tok)
	_, _ = a.WriteTo([]byte("a real payload"), nil)
	expectPayload(t, b, []byte("a real payload"))
	if got := srv.Metrics().TransfersCappedTotal; got != 0 {
		t.Errorf("transfers_capped_total = %d, want 0 (cap disabled)", got)
	}
}

func TestAllowReply_RateCap(t *testing.T) {
	s := &Server{}
	base := time.Unix(1700000000, 0)

	// The whole per-second budget is allowed.
	for i := 0; i < stunResponsesPerSec; i++ {
		if !s.allowReply(base) {
			t.Fatalf("response %d denied within budget", i)
		}
	}
	// One more in the same window is denied.
	if s.allowReply(base) {
		t.Fatal("expected denial after budget exhausted")
	}
	// A new one-second window resets the budget.
	if !s.allowReply(base.Add(time.Second)) {
		t.Fatal("expected allow after window rollover")
	}
}

func TestCookie_BoundToSourceAndExpires(t *testing.T) {
	s := NewServer(nil, ServerConfig{})
	tok := Token{7}
	src := &net.UDPAddr{IP: net.IPv4(192, 0, 2, 1), Port: 4000}
	now := time.Unix(1700000000, 0)
	c := s.cookie(tok, src, cookieEpoch(now))
	if !s.cookieValid(c, tok, src, now.Add(cookieLifetime)) {
		t.Error("cookie should survive into the next epoch")
	}
	if s.cookieValid(c, tok, src, now.Add(3*cookieLifetime)) {
		t.Error("cookie should expire after two epochs")
	}
	if s.cookieValid(c, tok, &net.UDPAddr{IP: src.IP, Port: 4001}, now) {
		t.Error("cookie must not validate for another port")
	}
	if s.cookieValid(c, Token{8}, src, now) {
		t.Error("cookie must not validate for another token")
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
