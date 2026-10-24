package relay

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/maphash"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/stun/v4"
)

// Server is the relay half of `fsend server`.
//
// It owns a single net.PacketConn (the UDP/443 listener). For each
// allocation, it tracks the two peers' addresses and forwards traffic
// between them. A peer's address is recorded only after it echoes a
// cookie sent to that address (see handleControl), so a spoofed source
// can't register. Peers whose network blocks UDP attach over a WebSocket
// instead (ws.go); both kinds pair freely. The wire format is documented
// in framing.go.
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

	// cookieKey keys the registration cookies (see cookie).
	cookieKey [32]byte

	// life ends WebSocket links when Run returns.
	life     context.Context
	stopLife context.CancelFunc

	// STUN responses and registration challenges are sent to a spoofable
	// source. A global fixed-window cap bounds the aggregate output, and a
	// per-prefix cap stops one source network from starving everyone. The
	// prefix table is fixed-size and keyed by a seeded hash, so spoofed
	// sources can't grow memory or aim at a victim's slot.
	stunMu      sync.Mutex
	stunWindow  time.Time
	stunCount   int
	stunSeed    maphash.Seed
	stunSeeded  bool
	stunPerSlot [stunPrefixSlots]uint16

	// healthy is true while the read loop is forwarding; it flips false if
	// Run exits on a real socket error (not a graceful ctx cancel) so the
	// signaling layer's /health can report the zombie instead of lying.
	healthy atomic.Bool
	// draining rejects new allocations during graceful shutdown so the drain
	// can converge while in-flight sessions finish.
	draining atomic.Bool

	// budget is the global per-UTC-day byte ceiling (Denial-of-Wallet
	// circuit breaker). Zero limit = unlimited.
	budget dayBudget

	// Aggregate counters for /metrics (cumulative since boot).
	bytesForwarded    atomic.Uint64
	peakTransferBytes atomic.Uint64 // most any single transfer has forwarded
	transfersCapped   atomic.Uint64 // transfers cut off for exceeding the per-session byte cap
	transfersTotal    atomic.Uint64 // transfers that forwarded ≥1 byte; vs paired sessions = relay-fallback rate
	oversize          atomic.Uint64 // datagrams dropped for exceeding MaxDatagram
	tcpFallbacks      atomic.Uint64 // WebSocket (TCP fallback) links accepted
}

// Metrics is an aggregate snapshot of relay activity for /metrics.
type Metrics struct {
	Forwarding           bool   `json:"forwarding"`
	Healthy              bool   `json:"healthy"`
	TransfersActive      int    `json:"transfers_active"`
	TransfersTotal       uint64 `json:"transfers_total"`
	TransfersCappedTotal uint64 `json:"transfers_capped_total"`
	BytesForwardedTotal  uint64 `json:"bytes_forwarded_total"`
	PeakTransferBytes    uint64 `json:"peak_transfer_bytes"`
	// Live usage only; the configured limit is not exposed on purpose.
	BudgetBytesToday       uint64 `json:"budget_bytes_today"`
	DatagramsOversizeTotal uint64 `json:"datagrams_oversize_total"`
	TCPFallbackTotal       uint64 `json:"tcp_fallback_total"`
}

// Metrics returns the current aggregate snapshot.
func (s *Server) Metrics() Metrics {
	return Metrics{
		Forwarding:             !s.cfg.DisableForwarding,
		Healthy:                s.healthy.Load(),
		TransfersActive:        s.ActiveAllocations(),
		TransfersTotal:         s.transfersTotal.Load(),
		TransfersCappedTotal:   s.transfersCapped.Load(),
		BytesForwardedTotal:    s.bytesForwarded.Load(),
		PeakTransferBytes:      s.peakTransferBytes.Load(),
		BudgetBytesToday:       s.budget.usedToday(time.Now()),
		DatagramsOversizeTotal: s.oversize.Load(),
		TCPFallbackTotal:       s.tcpFallbacks.Load(),
	}
}

// activeWindow bounds how recently an allocation must have forwarded a
// datagram to count as in-flight for drain purposes. Long enough to span
// QUIC's pacing gaps, short enough that drain converges once transfers stop.
const activeWindow = 3 * time.Second

// stunResponsesPerSec caps aggregate STUN binding replies. A few dozen
// small packets per pairing is plenty for ICE gathering, so this leaves
// real clients ample headroom while keeping the reflector's contribution
// to any amplification attack negligible (~160 KB/s of responses).
const stunResponsesPerSec = 4000

// stunPerPrefixPerSec caps replies per source /24 (IPv4) or /48 (IPv6),
// counted in stunPrefixSlots hashed buckets. Generous for a busy NAT, yet
// one network alone can't reach the global cap.
const (
	stunPerPrefixPerSec = 100
	stunPrefixSlots     = 4096
)

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
	// MaxBytesPerSession caps wire bytes a single session may forward.
	// 0 means unlimited — the default; the cap is opt-in via the env
	// layer in cmd/fsend (FSEND_RELAY_MAX_BYTES_PER_SESSION).
	MaxBytesPerSession uint64
	// MaxBytesPerDay caps wire bytes the relay forwards per UTC day across
	// all sessions — the Denial-of-Wallet ceiling. 0 means unlimited.
	MaxBytesPerDay uint64
	Logger         *slog.Logger
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

// Default fills in zero values. MaxBytesPerSession is deliberately not
// defaulted here — 0 means unlimited, which is also what the env layer
// passes for an unset var; the cap is opt-in.
func (c *ServerConfig) Default() {
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// allocation is one in-memory session, accessed by token under the
// allocation's own mutex so the hot path can run lock-free on the
// table-level RWMutex (only re-locked when adding/removing entries).
type allocation struct {
	token Token
	// peers are the two registered sides. Read lock-free on the hot path;
	// regMu serializes the rare writes in register.
	peers        [2]atomic.Pointer[peer]
	regMu        sync.Mutex
	bytes        atomic.Uint64
	lastActivity atomic.Int64 // unix nanos
	lastNudge    atomic.Int64 // unix nanos of the last unsolicited challenge
	createdAt    time.Time
	// dataCounted: true once this allocation forwards its first byte, so
	// transfersTotal counts it exactly once.
	dataCounted atomic.Bool
}

// peer is one registered side of an allocation.
type peer struct {
	id PeerID
	ep endpoint
}

// endpoint is where a peer receives: a verified UDP address, or a
// WebSocket link (TCP fallback). Exactly one field is set.
type endpoint struct {
	udp *net.UDPAddr
	ws  *wsLink
}

func (e endpoint) is(o endpoint) bool {
	if e.ws != nil || o.ws != nil {
		return e.ws == o.ws
	}
	return udpEqual(e.udp, o.udp)
}

// register records ep as the address of peer id: it replaces that peer's
// previous address (rebinding) or takes a free slot. It reports false
// when both slots belong to other peers — the allocation is 1↔1.
func (a *allocation) register(id PeerID, ep endpoint) bool {
	a.regMu.Lock()
	defer a.regMu.Unlock()
	np := &peer{id: id, ep: ep}
	for i := range a.peers {
		if p := a.peers[i].Load(); p != nil && p.id == id {
			a.peers[i].Store(np)
			return true
		}
	}
	for i := range a.peers {
		if a.peers[i].Load() == nil {
			a.peers[i].Store(np)
			return true
		}
	}
	return false
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
	if _, err := rand.Read(s.cookieKey[:]); err != nil {
		panic("relay: cookie key: " + err.Error()) // crypto/rand never fails on supported platforms
	}
	s.life, s.stopLife = context.WithCancel(context.Background())
	s.budget.limit = cfg.MaxBytesPerDay
	s.healthy.Store(true) // assume up until Run proves otherwise
	return s
}

// Healthy reports whether the relay read loop is still forwarding. It flips
// false only when Run exits on a real socket error, so /health can surface
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
		return Token{}, ErrDraining
	}
	// Refuse up front when the day's budget is already spent, so both peers
	// fail fast with a clear reason. (A budget that trips mid-transfer is
	// handled in handle, via the same eviction path as the per-session cap.)
	if s.budget.exhausted(time.Now()) {
		return Token{}, ErrBudgetExhausted
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
	// transfersTotal is bumped on first forwarded byte (see handle), not here:
	// every pairing allocates a slot for STUN, so allocations aren't relay use.
	return t, nil
}

// Run drives the UDP read loops until ctx cancels or conn closes, then
// ends any WebSocket links.
//
// This is the hot path: one syscall per datagram, one hashmap read, one
// syscall to forward. Several readers share the socket so one reader's
// forwarding write overlaps another's read.
func (s *Server) Run(ctx context.Context) error {
	defer s.stopLife()
	go s.janitor(ctx)

	go func() {
		<-ctx.Done()
		_ = s.conn.SetReadDeadline(time.Now())
	}()

	n := min(runtime.GOMAXPROCS(0), maxReaders)
	errs := make(chan error, n)
	for range n {
		go func() { errs <- s.readLoop(ctx) }()
	}
	var first error
	for range n {
		if err := <-errs; err != nil && first == nil {
			first = err
			_ = s.conn.SetReadDeadline(time.Now()) // stop the siblings too
		}
	}
	return first
}

// maxReaders caps the read loops in Run. Reads on one socket serialize in
// the kernel, so more readers than this buy nothing.
const maxReaders = 4

func (s *Server) readLoop(ctx context.Context) error {
	// One byte over MaxDatagram, so a datagram the buffer would truncate
	// is detected and dropped instead of forwarded cut short.
	buf := make([]byte, MaxDatagram+1)
	readErrs := 0
	for {
		n, addr, err := s.conn.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// Transient errors (e.g. ENOBUFS) are retried; a closed socket or
			// a persistent failure means the relay is dead. Flag it so /health
			// stops reporting a healthy zombie, and let the caller exit.
			readErrs++
			if errors.Is(err, net.ErrClosed) || readErrs > maxReadErrors {
				s.healthy.Store(false)
				return fmt.Errorf("relay: read: %w", err)
			}
			s.logger.Debug("relay: transient read error", "err", err)
			time.Sleep(readErrBackoff)
			continue
		}
		readErrs = 0
		if n > MaxDatagram {
			s.oversize.Add(1)
			continue
		}
		ua, ok := addr.(*net.UDPAddr)
		if !ok {
			continue
		}
		// Process inline — no goroutine per datagram. The buffer is reused,
		// so anything handle keeps (a WebSocket send) must copy.
		s.handleSafe(buf[:n], endpoint{udp: ua})
	}
}

// handleSafe runs handle with panic recovery so one malformed datagram can
// never crash the whole server (and every other live session with it).
func (s *Server) handleSafe(datagram []byte, src endpoint) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("relay: recovered panic handling datagram", "panic", r)
		}
	}()
	s.handle(datagram, src)
}

func (s *Server) handle(datagram []byte, src endpoint) {
	if len(datagram) == 0 {
		return
	}
	// The socket carries three wire formats, demuxed by the first byte:
	// relay data (0x01), relay control (0x02), and STUN, whose first byte
	// has its top two bits clear (RFC 5389 §6).
	switch datagram[0] {
	case ProtocolVersion:
	case ControlVersion:
		if !s.cfg.DisableForwarding {
			s.handleControl(datagram, src)
		}
		return
	default:
		if src.udp != nil {
			s.handleSTUN(datagram, src.udp)
		}
		return
	}
	if s.cfg.DisableForwarding {
		return // stun-only mode: STUN answered above, data never carried
	}
	_, token, _, ok := Parse(datagram)
	if !ok {
		return // silently drop malformed
	}
	a := s.lookup(token)
	if a == nil {
		return // unknown token = silently drop (likely stale)
	}
	now := time.Now()

	// Determine forward direction. Only registered sources forward.
	var dst *peer
	p0, p1 := a.peers[0].Load(), a.peers[1].Load()
	switch {
	case p0 != nil && p0.ep.is(src):
		dst = p1
	case p1 != nil && p1.ep.is(src):
		dst = p0
	default:
		// Unknown source with a live token: most likely a registered peer
		// whose NAT rebound. Challenge it so it can re-register from here.
		// The reply is no larger than this datagram, so it can't amplify.
		if src.udp != nil && len(datagram) >= ControlSize && a.nudgeDue(now) {
			s.challenge(token, PeerID{}, src.udp, now)
		}
		return
	}
	a.lastActivity.Store(now.UnixNano())
	if dst == nil {
		return // still waiting for the second peer
	}

	// Byte cap check (counts header + payload — wire bytes, what we pay for).
	wireSize := uint64(len(datagram))
	total := a.bytes.Add(wireSize)
	if s.cfg.MaxBytesPerSession > 0 && total > s.cfg.MaxBytesPerSession {
		s.transfersCapped.Add(1)
		s.evict(token, ReasonCapHit)
		return
	}

	// Daily budget (Denial-of-Wallet circuit breaker): once the day's bytes
	// are spent, evict and drop until the UTC-midnight rollover.
	if s.budget.charge(now, wireSize) {
		s.evict(token, ReasonBudgetHit)
		return
	}

	s.bytesForwarded.Add(wireSize)
	if a.dataCounted.CompareAndSwap(false, true) {
		s.transfersTotal.Add(1) // first byte forwarded = this allocation is a real transfer
	}
	for { // raise the global peak to this transfer's running total
		cur := s.peakTransferBytes.Load()
		if total <= cur || s.peakTransferBytes.CompareAndSwap(cur, total) {
			break
		}
	}
	s.send(dst.ep, datagram)
}

// send delivers b to ep. A full WebSocket queue drops b, like a full
// socket buffer would; QUIC recovers.
func (s *Server) send(ep endpoint, b []byte) {
	if ep.ws != nil {
		ep.ws.send(b)
		return
	}
	if _, err := s.conn.WriteTo(b, ep.udp); err != nil {
		s.logger.Debug("relay: write failed", "err", err)
	}
}

func (s *Server) lookup(t Token) *allocation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.allocs[t]
}

// handleControl runs the registration handshake. Over UDP, HELLO gets a
// CHALLENGE carrying a cookie bound to the source address, and only a
// REGISTER echoing a valid cookie records that address — so a spoofed
// source never sees the cookie and can't aim the relay at a third party.
// A WebSocket source is already return-routable (TCP), so its HELLO
// registers directly.
func (s *Server) handleControl(b []byte, src endpoint) {
	tok, typ, id, cookie, ok := parseControl(b)
	if !ok {
		return
	}
	a := s.lookup(tok)
	if a == nil {
		return
	}
	now := time.Now()
	switch {
	case typ == msgHello && src.udp != nil:
		s.challenge(tok, id, src.udp, now)
		return
	case typ == msgHello: // WebSocket
	case typ == msgRegister:
		if src.udp != nil && !s.cookieValid(cookie, tok, src.udp, now) {
			return
		}
	default:
		return
	}
	if !a.register(id, src) {
		return // both slots taken by other peers
	}
	a.lastActivity.Store(now.UnixNano())
	s.send(src, frameControl(tok, msgRegistered, id, nil))
}

// challenge sends a CHALLENGE to src, within the global reply cap.
func (s *Server) challenge(tok Token, id PeerID, src *net.UDPAddr, now time.Time) {
	if !s.allowReply(now, src.IP) {
		return
	}
	c := s.cookie(tok, src, cookieEpoch(now))
	s.send(endpoint{udp: src}, frameControl(tok, msgChallenge, id, &c))
}

// cookieLifetime is the cookie epoch; a cookie stays valid for one to two
// epochs, plenty for one round trip and short enough to go stale fast.
const cookieLifetime = 30 * time.Second

func cookieEpoch(now time.Time) uint64 { return uint64(now.UnixNano() / int64(cookieLifetime)) }

// cookie is HMAC(key, epoch‖token‖src)[:16], stateless like a TURN nonce
// or a QUIC retry token.
func (s *Server) cookie(tok Token, src *net.UDPAddr, epoch uint64) [cookieSize]byte {
	m := hmac.New(sha256.New, s.cookieKey[:])
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], epoch)
	m.Write(b[:])
	m.Write(tok[:])
	m.Write(src.IP.To16())
	binary.BigEndian.PutUint16(b[:2], uint16(src.Port))
	m.Write(b[:2])
	var out [cookieSize]byte
	copy(out[:], m.Sum(nil))
	return out
}

func (s *Server) cookieValid(c [cookieSize]byte, tok Token, src *net.UDPAddr, now time.Time) bool {
	e := cookieEpoch(now)
	for _, epoch := range []uint64{e, e - 1} {
		want := s.cookie(tok, src, epoch)
		if hmac.Equal(c[:], want[:]) {
			return true
		}
	}
	return false
}

// nudgeInterval rate-limits unsolicited challenges per allocation: a
// rebinding peer keeps sending, and one challenge per burst is enough.
const nudgeInterval = 100 * time.Millisecond

func (a *allocation) nudgeDue(now time.Time) bool {
	last := a.lastNudge.Load()
	return now.UnixNano()-last >= int64(nudgeInterval) &&
		a.lastNudge.CompareAndSwap(last, now.UnixNano())
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
	if !s.allowReply(time.Now(), src.IP) {
		return // cap hit; drop silently (ICE clients retry/fall back)
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

// allowReply reports whether a STUN reply or registration challenge to ip
// may be sent, enforcing the per-prefix and global one-second windows.
// Fixed memory.
func (s *Server) allowReply(now time.Time, ip net.IP) bool {
	s.stunMu.Lock()
	defer s.stunMu.Unlock()
	if !s.stunSeeded {
		s.stunSeed, s.stunSeeded = maphash.MakeSeed(), true
	}
	if now.Sub(s.stunWindow) >= time.Second {
		s.stunWindow = now
		s.stunCount = 0
		clear(s.stunPerSlot[:])
	}
	slot := &s.stunPerSlot[maphash.Bytes(s.stunSeed, stunPrefix(ip))%stunPrefixSlots]
	if s.stunCount >= stunResponsesPerSec || *slot >= stunPerPrefixPerSec {
		return false
	}
	s.stunCount++
	*slot++
	return true
}

// stunPrefix is the source network a reply is charged to: the /24 for
// IPv4, the /48 for IPv6.
func stunPrefix(ip net.IP) []byte {
	if v4 := ip.To4(); v4 != nil {
		return v4[:3]
	}
	if len(ip) == net.IPv6len {
		return ip[:6]
	}
	return ip
}

// ErrDraining is returned by Allocate during graceful shutdown.
var ErrDraining = errors.New("relay: server is draining")

// maxReadErrors consecutive read errors, readErrBackoff apart (~5s), make
// Run give up so the process can exit and be restarted.
const (
	maxReadErrors  = 100
	readErrBackoff = 50 * time.Millisecond
)

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
	ReasonCapHit    = "cap_hit"
	ReasonBudgetHit = "daily_budget"
	ReasonIdle      = "idle"
)

// ErrBudgetExhausted is returned by Allocate when the daily byte budget is
// already spent, so the signaling layer can flag the response and clients
// can fail fast with a specific reason.
var ErrBudgetExhausted = errors.New("relay: daily byte budget exhausted")

// udpEqual reports whether two UDPAddrs refer to the same endpoint.
func udpEqual(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}
