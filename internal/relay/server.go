package relay

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v3"
)

// Server is the UDP-relay half of `fsend server`.
//
// It owns a single net.PacketConn (the UDP/443 listener). For each
// allocation, it tracks the two peers' addresses (set on first datagram
// from each) and forwards traffic between them. The wire format is
// documented in framing.go.
//
// The same socket also answers STUN binding requests so ICE clients can
// gather server-reflexive candidates without a second server or port;
// see handleSTUN.
type Server struct {
	conn   net.PacketConn
	cfg    ServerConfig
	logger *slog.Logger

	mu        sync.RWMutex
	allocs    map[Token]*allocation
	tombstone map[Token]tombstoneEntry // recently evicted tokens + reason; janitor expires by age

	// STUN responses are a small (~2x) amplifier off a spoofable source.
	// A global fixed-window cap bounds the reflector's aggregate output;
	// it is deliberately NOT per-source, because the source IP is forgeable
	// and a per-source map would itself be an unbounded-memory vector.
	stunMu     sync.Mutex
	stunWindow time.Time
	stunCount  int

	// healthy is true while the read loop is forwarding; it flips false if
	// Run exits on a real socket error (not a graceful ctx cancel) so the
	// signaling layer's /v1/health can report the zombie instead of lying.
	healthy atomic.Bool
	// draining rejects new allocations during graceful shutdown so the drain
	// can converge while in-flight sessions finish.
	draining atomic.Bool
}

// activeWindow bounds how recently an allocation must have forwarded a
// datagram to count as in-flight for drain purposes. Long enough to span
// QUIC's pacing gaps, short enough that drain converges once transfers stop.
const activeWindow = 3 * time.Second

// stunResponsesPerSec caps aggregate STUN binding replies. A few dozen
// small packets per pairing is plenty for ICE gathering, so this leaves
// real clients ample headroom while keeping the reflector's contribution
// to any amplification attack negligible (~40 KB/s of responses).
const stunResponsesPerSec = 1000

// tombstoneEntry records why an allocation was evicted, with the
// monotonic-ish unix-nanos timestamp the janitor uses to expire it.
// A bare reason string (previous shape) couldn't be aged out, so we
// were forced to wipe the entire map under a 1024-entry safety valve —
// losing every status lookup during a burst.
type tombstoneEntry struct {
	reason    string
	expiresAt int64 // unix nanos
}

// TombstoneTTL is how long an evicted token's reason stays around for
// status queries. Long enough that a CLI's "transfer dropped → probe
// status" round-trip lands on the tombstone, short enough that the
// map doesn't grow without bound.
const TombstoneTTL = 5 * time.Minute

// ServerConfig holds the per-session tuning.
type ServerConfig struct {
	MaxBytesPerSession uint64
	Logger             *slog.Logger
	// DisableForwarding makes the relay answer STUN but never carry data
	// (pairing + hole-punching only). Negative so the zero value forwards.
	DisableForwarding bool
}

// janitorInterval is how often the eviction loop sweeps idle
// allocations. Not exposed in ServerConfig because no caller sets it.
const janitorInterval = 30 * time.Second

// sessionIdleTimeout is the per-allocation silence ceiling before the
// janitor evicts. QUIC's own MaxIdleTimeout (30s) fires first in any
// healthy transfer, so this is a backstop for cleanup, not a knob —
// raising or lowering it doesn't affect well-behaved sessions.
const sessionIdleTimeout = 60 * time.Second

// Default fills in zero values.
func (c *ServerConfig) Default() {
	if c.MaxBytesPerSession == 0 {
		c.MaxBytesPerSession = 100 * 1000 * 1000 // 100 MB (decimal, matches displayed units)
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// allocation is one in-memory session, accessed by token under the
// allocation's own mutex so the hot path can run lock-free on the
// table-level RWMutex (only re-locked when adding/removing entries).
type allocation struct {
	token        Token
	peerA, peerB atomic.Pointer[net.UDPAddr]
	bytes        atomic.Uint64
	lastActivity atomic.Int64 // unix nanos
	createdAt    time.Time
}

// NewServer constructs a Server bound to the given net.PacketConn.
// Caller is responsible for opening the conn; we don't manage its
// lifecycle except for Close in Stop.
func NewServer(conn net.PacketConn, cfg ServerConfig) *Server {
	cfg.Default()
	s := &Server{
		conn:      conn,
		cfg:       cfg,
		logger:    cfg.Logger,
		allocs:    make(map[Token]*allocation),
		tombstone: make(map[Token]tombstoneEntry),
	}
	s.healthy.Store(true) // assume up until Run proves otherwise
	return s
}

// Healthy reports whether the relay read loop is still forwarding. It flips
// false only when Run exits on a real socket error, so /v1/health can surface
// a dead relay rather than reporting a healthy zombie.
func (s *Server) Healthy() bool { return s.healthy.Load() }

// Drain stops the relay from accepting new allocations so a graceful shutdown
// can wait for existing in-flight sessions to finish before the socket closes.
func (s *Server) Drain() { s.draining.Store(true) }

// ActiveAllocations counts sessions that forwarded a datagram within
// activeWindow — i.e. transfers currently moving bytes. Idle reservations
// (allocated but unused) don't block a drain.
func (s *Server) ActiveAllocations() int {
	cutoff := time.Now().Add(-activeWindow).UnixNano()
	s.mu.RLock()
	defer s.mu.RUnlock()
	n := 0
	for _, a := range s.allocs {
		if a.lastActivity.Load() >= cutoff {
			n++
		}
	}
	return n
}

// MaxBytesPerSession exposes the configured byte ceiling so the
// signaling layer can include it in the relay-status response.
func (s *Server) MaxBytesPerSession() uint64 {
	return s.cfg.MaxBytesPerSession
}

// Forwarding reports whether the relay carries data, so the signaling
// layer can tell clients to skip the relay fallback when it doesn't.
func (s *Server) Forwarding() bool {
	return !s.cfg.DisableForwarding
}

// Status returns the eviction reason for a token, or "" if the
// allocation is still live or unknown. Used by the signaling layer's
// /v1/relay/status endpoint so the CLI can surface the real reason a
// relay-path transfer dropped (e.g. cap_hit) instead of a generic
// "connection interrupted, retrying" loop.
func (s *Server) Status(t Token) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, live := s.allocs[t]; live {
		return ""
	}
	if entry, ok := s.tombstone[t]; ok {
		return entry.reason
	}
	return ""
}

// Allocate creates a new relay slot and returns the token clients embed
// in their datagrams. Called from the signaling layer's
// POST /v1/relay/allocate handler.
func (s *Server) Allocate() (Token, error) {
	if s.draining.Load() {
		return Token{}, errors.New("relay: server is draining")
	}
	var t Token
	if _, err := rand.Read(t[:]); err != nil {
		return Token{}, fmt.Errorf("relay: token rand: %w", err)
	}
	now := time.Now()
	a := &allocation{token: t, createdAt: now}
	a.lastActivity.Store(now.UnixNano())
	s.mu.Lock()
	if _, exists := s.allocs[t]; exists {
		s.mu.Unlock()
		return Token{}, errors.New("relay: token collision (extremely unlikely)")
	}
	s.allocs[t] = a
	s.mu.Unlock()
	return t, nil
}

// Run drives the UDP read loop until ctx cancels or conn closes.
//
// This is the hot path: one syscall per datagram, one hashmap read, one
// syscall to forward. Buffers are reused via sync.Pool to avoid GC churn.
func (s *Server) Run(ctx context.Context) error {
	bufPool := &sync.Pool{New: func() any { b := make([]byte, 1500); return &b }}

	go s.janitor(ctx)

	go func() {
		<-ctx.Done()
		_ = s.conn.SetReadDeadline(time.Now())
	}()

	for {
		bufPtr := bufPool.Get().(*[]byte)
		buf := *bufPtr
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			bufPool.Put(bufPtr)
			if ctx.Err() != nil {
				return nil
			}
			// Other read errors (e.g. socket closed): the relay is dead.
			// Flag it so /v1/health stops reporting a healthy zombie.
			s.healthy.Store(false)
			return fmt.Errorf("relay: read: %w", err)
		}
		// Process inline — no goroutine per datagram (would be massive
		// overhead). Forward is one syscall. Recover per datagram so a
		// crafted packet that trips a panic in handle can't take the whole
		// server down with it.
		s.handleSafe(buf[:n], addr.(*net.UDPAddr))
		bufPool.Put(bufPtr)
	}
}

// handleSafe runs handle with panic recovery so one malformed datagram can
// never crash the whole server (and every other live session with it).
func (s *Server) handleSafe(datagram []byte, src *net.UDPAddr) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("relay: recovered panic handling datagram", "panic", r)
		}
	}()
	s.handle(datagram, src)
}

func (s *Server) handle(datagram []byte, src *net.UDPAddr) {
	// The socket carries two wire formats: relay frames start with
	// ProtocolVersion (0x01), STUN requests with 0x00 (RFC 5389 §6) —
	// the first byte demuxes them exactly.
	if len(datagram) > 0 && datagram[0] != ProtocolVersion {
		s.handleSTUN(datagram, src)
		return
	}
	if s.cfg.DisableForwarding {
		return // stun-only mode: STUN answered above, data never carried
	}
	ver, token, _, ok := Parse(datagram)
	if !ok || ver != ProtocolVersion {
		return // silently drop malformed
	}

	s.mu.RLock()
	a, found := s.allocs[token]
	s.mu.RUnlock()
	if !found {
		return // unknown token = silently drop (likely stale)
	}

	a.lastActivity.Store(time.Now().UnixNano())

	// Determine forward direction.
	var dst *net.UDPAddr
	peerA := a.peerA.Load()
	peerB := a.peerB.Load()
	switch {
	case peerA == nil:
		// First datagram on this allocation; register sender as peerA.
		a.peerA.Store(src)
		// peerB still unknown; nothing to forward to yet.
		return
	case udpEqual(src, peerA):
		if peerB == nil {
			return // still waiting for second peer
		}
		dst = peerB
	case peerB == nil:
		// Second peer registers themselves.
		a.peerB.Store(src)
		dst = peerA
	case udpEqual(src, peerB):
		dst = peerA
	default:
		// Third-party trying to inject; silently drop.
		return
	}

	// Byte cap check (counts header + payload — wire bytes, what we pay for).
	wireSize := uint64(len(datagram))
	total := a.bytes.Add(wireSize)
	if s.cfg.MaxBytesPerSession > 0 && total > s.cfg.MaxBytesPerSession {
		s.evict(token, ReasonCapHit)
		return
	}

	if _, err := s.conn.WriteTo(datagram, dst); err != nil {
		s.logger.Debug("relay: write failed", "err", err)
	}
}

// handleSTUN answers a STUN binding request with the source's reflexive
// address (RFC 5389). This is what lets ICE gather srflx candidates and
// hole-punch through NATs instead of relaying; see internal/iceconn.
//
// Unauthenticated by design, like any public STUN server: the response
// is barely larger than the request, so there's no amplification value,
// and it discloses only the querier's own address.
func (s *Server) handleSTUN(datagram []byte, src *net.UDPAddr) {
	if !stun.IsMessage(datagram) {
		return
	}
	if !s.allowSTUNResponse(time.Now()) {
		return // aggregate cap hit; drop silently (ICE clients retry/fall back)
	}
	req := &stun.Message{Raw: datagram}
	if err := req.Decode(); err != nil || req.Type != stun.BindingRequest {
		return
	}
	resp, err := stun.Build(req, stun.BindingSuccess,
		&stun.XORMappedAddress{IP: src.IP, Port: src.Port},
		stun.Fingerprint)
	if err != nil {
		return
	}
	if _, err := s.conn.WriteTo(resp.Raw, src); err != nil {
		s.logger.Debug("relay: stun write failed", "err", err)
	}
}

// allowSTUNResponse reports whether a STUN reply may be sent, enforcing a
// global fixed-window rate cap. Cheap (one mutex, two fields) and holds no
// per-source state, so it can't be turned into a memory-exhaustion vector
// by source spoofing.
func (s *Server) allowSTUNResponse(now time.Time) bool {
	s.stunMu.Lock()
	defer s.stunMu.Unlock()
	if now.Sub(s.stunWindow) >= time.Second {
		s.stunWindow = now
		s.stunCount = 0
	}
	if s.stunCount >= stunResponsesPerSec {
		return false
	}
	s.stunCount++
	return true
}

func (s *Server) evict(t Token, reason string) {
	s.mu.Lock()
	delete(s.allocs, t)
	s.tombstone[t] = tombstoneEntry{
		reason:    reason,
		expiresAt: time.Now().Add(TombstoneTTL).UnixNano(),
	}
	s.mu.Unlock()
	s.logger.Debug("relay: evicted", "token", t.String(), "reason", reason)
}

func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(janitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.sweep(now)
		}
	}
}

// sweep evicts idle allocations and expired tombstones. Wrapped in panic
// recovery so a bug here can't silently kill the janitor goroutine and let
// the maps grow unbounded.
func (s *Server) sweep(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("relay: recovered panic in janitor", "panic", r)
		}
	}()
	cutoff := now.Add(-sessionIdleTimeout).UnixNano()
	nowNanos := now.UnixNano()
	s.mu.Lock()
	defer s.mu.Unlock()
	for tok, a := range s.allocs {
		if a.lastActivity.Load() < cutoff {
			delete(s.allocs, tok)
			s.tombstone[tok] = tombstoneEntry{
				reason:    ReasonIdle,
				expiresAt: now.Add(TombstoneTTL).UnixNano(),
			}
		}
	}
	// Expire tombstones by per-entry age — no bulk-wipe, so a burst of
	// evictions can't blind every subsequent status lookup.
	for tok, entry := range s.tombstone {
		if entry.expiresAt < nowNanos {
			delete(s.tombstone, tok)
		}
	}
}

// Eviction reasons surfaced through Status().
const (
	ReasonCapHit = "cap_hit"
	ReasonIdle   = "idle"
)

// udpEqual reports whether two UDPAddrs refer to the same endpoint.
func udpEqual(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}
