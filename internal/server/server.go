package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/relay"
)

// Config holds runtime tuning for the signaling server.
type Config struct {
	ServerVersion        string
	UnpairedTTL          time.Duration // default 1h
	AbandonedTTL         time.Duration // default 5m
	PairedTTL            time.Duration // default 600s
	LongPollTimeout      time.Duration // default 25s
	MaxSessionsPerIP     int           // default 5
	MaxNewSessionsPerMin int           // default 30
	Logger               *slog.Logger

	// ServerPassword, when non-empty, gates every endpoint except
	// /v1/health behind a constant-time match on the X-Fsend-Auth
	// header. Empty leaves the server open (the default for the public
	// fsend.alzina.dev pairing server and for the canonical Docker stack).
	ServerPassword string
}

// Default fills in zero values with sensible defaults.
func (c *Config) Default() {
	if c.UnpairedTTL == 0 {
		// One hour. The sender is expected to hold the terminal open for
		// as long as they're waiting for a receiver; a tight cap here
		// would surface as a confusing "code expired" failure even when
		// the sender's process is still alive.
		c.UnpairedTTL = time.Hour
	}
	if c.AbandonedTTL == 0 {
		// Live senders touch their session every /wait long-poll
		// (≤LongPollTimeout apart). A session not seen for 5 minutes
		// has a dead client — reclaim it so a crashed or killed fsend
		// doesn't hold one of the MaxSessionsPerIP slots for the full
		// UnpairedTTL hour.
		c.AbandonedTTL = 5 * time.Minute
	}
	if c.PairedTTL == 0 {
		c.PairedTTL = 600 * time.Second
	}
	if c.LongPollTimeout == 0 {
		c.LongPollTimeout = 25 * time.Second
	}
	if c.MaxSessionsPerIP == 0 {
		c.MaxSessionsPerIP = 5
	}
	if c.MaxNewSessionsPerMin == 0 {
		c.MaxNewSessionsPerMin = 30
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
}

// Resource limits on signaling requests. Constants rather than Config
// fields because no operator should ever need to tune them — they exist
// to keep one buggy or malicious peer from exhausting server memory,
// not to express a product knob.
const (
	// maxRequestBodyBytes caps any inbound JSON body. Every legitimate
	// request payload (CreateSessionRequest, JoinSessionRequest,
	// CandidatesPushRequest, RelayAllocateRequest, WaitRequest) is well
	// under 1 KiB; 16 KiB leaves several orders of magnitude of headroom
	// for ICE candidate batches without admitting a multi-MB push.
	maxRequestBodyBytes = 16 * 1024

	// maxCandidatesPerSide bounds how many ICE candidates one peer can
	// accumulate on the pairing server before further pushes are rejected.
	// pion typically gathers fewer than 10 (host + srflx pairs across
	// interfaces); 64 is generous and still cheap to keep in memory.
	maxCandidatesPerSide = 64
)

// Server implements the HTTP signaling layer.
//
// Concurrency model: a single sync.Mutex guards the session table. The
// table is small (one entry per active pairing, sessions live <60s
// typically), so contention is negligible. A janitor goroutine sweeps
// expired entries every 10 seconds.
type Server struct {
	cfg      Config
	started  time.Time
	mu       sync.Mutex
	bySlot   map[string]*session
	byID     map[string]*session
	ipCounts map[string]int         // active sessions per source IP
	ipBucket map[string]*rateBucket // new-session rate limiter per source IP

	// Relay-fallback wiring (optional). When non-nil, the server exposes
	// POST /v1/relay/allocate and returns "host(request.Host):relayUDPPort"
	// — the same hostname the client used to reach signaling, paired with
	// the configured UDP port. No operator config needed for stock setups.
	relayAllocator RelayAllocator
	relayUDPPort   int
}

// RelayAllocator is the minimal interface internal/relay.Server exposes
// for our purposes — abstracting it lets us test the signaling layer
// without standing up real UDP sockets.
type RelayAllocator interface {
	Allocate() (relay.Token, error)
	Status(relay.Token) string
	MaxBytesPerSession() uint64
}

// WithRelay wires a relay allocator into the signaling layer.
// udpPort is the UDP port the relay listens on; the public address
// advertised to clients is host(request.Host):udpPort.
func (s *Server) WithRelay(allocator RelayAllocator, udpPort int) *Server {
	s.relayAllocator = allocator
	s.relayUDPPort = udpPort
	return s
}

// relayAddrFor builds the host:port to advertise to a client by pairing
// the request's Host header (what the client used to reach signaling)
// with the relay's UDP port.
func (s *Server) relayAddrFor(r *http.Request) string {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return net.JoinHostPort(host, strconv.Itoa(s.relayUDPPort))
}

// New constructs a Server with defaults filled in.
func New(cfg Config) *Server {
	cfg.Default()
	return &Server{
		cfg:      cfg,
		started:  time.Now(),
		bySlot:   make(map[string]*session),
		byID:     make(map[string]*session),
		ipCounts: make(map[string]int),
		ipBucket: make(map[string]*rateBucket),
	}
}

// rateBucket is a tiny token bucket: at most N events per window.
type rateBucket struct {
	stamps []time.Time
}

func (b *rateBucket) allow(now time.Time, limit int, window time.Duration) bool {
	cutoff := now.Add(-window)
	keep := b.stamps[:0]
	for _, t := range b.stamps {
		if t.After(cutoff) {
			keep = append(keep, t)
		}
	}
	b.stamps = keep
	if len(b.stamps) >= limit {
		return false
	}
	b.stamps = append(b.stamps, now)
	return true
}

// AuthHeader is the HTTP header clients use to present a self-hosted
// server's shared password. Distinct from the Authorization: Bearer
// scheme, which carries the per-session role token issued by Create /
// Join — the two auth surfaces are independent.
const AuthHeader = "X-Fsend-Auth"

// Handler returns the HTTP handler the server should expose. When
// Config.ServerPassword is set, every endpoint except /v1/health is
// wrapped in a constant-time password check; /v1/health stays open so
// Docker HEALTHCHECK and monitoring don't need to share the secret.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/session", s.createSession)
	m.HandleFunc("POST /v1/session/{slot}/join", s.joinSession)
	m.HandleFunc("POST /v1/session/{slot}/wait", s.waitSession)
	m.HandleFunc("POST /v1/session/{id}/candidates", s.pushCandidates)
	m.HandleFunc("GET /v1/session/{id}/candidates", s.pullCandidates)
	m.HandleFunc("DELETE /v1/session/{id}", s.deleteSession)
	m.HandleFunc("POST /v1/relay/allocate", s.allocateRelay)
	m.HandleFunc("GET /v1/relay/status", s.relayStatus)
	m.HandleFunc("GET /v1/health", s.health)
	if s.cfg.ServerPassword == "" {
		return m
	}
	return s.withServerAuth(m)
}

// withServerAuth wraps inner with a header check that constant-time
// compares X-Fsend-Auth against the configured password. /v1/health
// passes through unconditionally.
func (s *Server) withServerAuth(inner http.Handler) http.Handler {
	want := []byte(s.cfg.ServerPassword)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/health" {
			inner.ServeHTTP(w, r)
			return
		}
		got := r.Header.Get(AuthHeader)
		if got == "" || subtle.ConstantTimeCompare(want, []byte(got)) != 1 {
			w.Header().Set("WWW-Authenticate", `X-Fsend-Auth realm="fsend"`)
			writeJSONError(w, http.StatusUnauthorized, "server password required")
			return
		}
		inner.ServeHTTP(w, r)
	})
}

// relayStatus reports the eviction reason for a relay allocation,
// keyed by the session's token. Used by the CLI to translate an opaque
// "relay path dropped" into an actionable message (cap hit, idle).
func (s *Server) relayStatus(w http.ResponseWriter, r *http.Request) {
	if s.relayAllocator == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "relay not enabled")
		return
	}
	id := r.URL.Query().Get("session_id")
	if id == "" {
		writeJSONError(w, http.StatusBadRequest, "missing session_id")
		return
	}
	s.mu.Lock()
	sess, ok := s.byID[id]
	// Bearer-gate the probe with the session's role token, matching
	// allocateRelay. A missing session and an unauthorized caller both
	// return "unknown" so this leaks no existence oracle to a third party
	// who only sniffed the session_id.
	if !ok || !sess.relayTokenSet {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, RelayStatusResponse{State: "unknown"})
		return
	}
	if _, authed := sess.sideForToken(bearerToken(r)); !authed {
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, RelayStatusResponse{State: "unknown"})
		return
	}
	tok := sess.relayToken
	s.mu.Unlock()
	reason := s.relayAllocator.Status(tok)
	if reason == "" {
		writeJSON(w, http.StatusOK, RelayStatusResponse{State: "active"})
		return
	}
	resp := RelayStatusResponse{State: "evicted", Reason: reason}
	if reason == relay.ReasonCapHit {
		resp.LimitBytes = s.relayAllocator.MaxBytesPerSession()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) allocateRelay(w http.ResponseWriter, r *http.Request) {
	if s.relayAllocator == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "relay not enabled on this server")
		return
	}
	var body RelayAllocateRequest
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json")
		return
	}
	if body.SessionID == "" {
		writeJSONError(w, http.StatusBadRequest, "missing session_id")
		return
	}
	tok := bearerToken(r)
	s.mu.Lock()
	sess, ok := s.byID[body.SessionID]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	// Bearer-gate the alloc: a session_id is a ULID and effectively
	// unguessable, but defense-in-depth — without this check, a leaked
	// or sniffed session_id alone would let a third party mint the relay
	// token and race the legitimate peer to register as peerA on the
	// relay's UDP demux.
	if _, ok := sess.sideForToken(tok); !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusUnauthorized, "missing or invalid role token")
		return
	}
	// One token per session: both peers must end up with the same value
	// so the relay's source-addr de-mux pairs them. Allocate lazily on
	// first call; subsequent calls reuse.
	if !sess.relayTokenSet {
		t, err := s.relayAllocator.Allocate()
		if err != nil {
			s.mu.Unlock()
			writeJSONError(w, http.StatusInternalServerError, "alloc failed")
			return
		}
		sess.relayToken = t
		sess.relayTokenSet = true
	}
	relayTok := sess.relayToken
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, RelayAllocateResponse{
		RelayAddr:    s.relayAddrFor(r),
		SessionToken: relayTok.String(),
		TTLSeconds:   600,
	})
}

// StartJanitor launches the background eviction loop. It exits when ctx
// is cancelled.
func (s *Server) StartJanitor(ctx context.Context) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			s.evictSafe(now)
		}
	}
}

// evictSafe runs one eviction sweep with panic recovery, so a bug in evict
// can't silently kill the janitor and let the session maps grow unbounded.
func (s *Server) evictSafe(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			s.cfg.Logger.Error("recovered panic in session janitor", "panic", r)
		}
	}()
	s.evict(now)
}

func (s *Server) evict(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.byID {
		switch sess.State {
		case "waiting":
			expired := now.Sub(sess.CreatedAt) > s.cfg.UnpairedTTL
			abandoned := now.Sub(sess.LastSeen) > s.cfg.AbandonedTTL
			if expired || abandoned {
				delete(s.byID, id)
				delete(s.bySlot, sess.Slot)
				s.releaseIP(sess.SenderRateKey)
				close(sess.waiters)
				reason := "expired"
				if abandoned && !expired {
					reason = "abandoned"
				}
				s.cfg.Logger.Debug("session evicted", "slot", sess.Slot, "reason", reason)
			}
		case "paired":
			if now.Sub(sess.PairedAt) > s.cfg.PairedTTL {
				delete(s.byID, id)
				delete(s.bySlot, sess.Slot)
				s.releaseIP(sess.SenderRateKey)
				s.releaseIP(sess.ReceiverRateKey)
				s.cfg.Logger.Debug("session evicted", "slot", sess.Slot, "reason", "paired_ttl")
			}
		}
	}
	// Drop rate-limit buckets whose stamps have all aged out. Without
	// this, every IP that ever connected lives in ipBucket forever — the
	// allowNewSession write path only creates entries.
	cutoff := now.Add(-time.Minute)
	for key, b := range s.ipBucket {
		keep := b.stamps[:0]
		for _, t := range b.stamps {
			if t.After(cutoff) {
				keep = append(keep, t)
			}
		}
		b.stamps = keep
		if len(b.stamps) == 0 {
			delete(s.ipBucket, key)
		}
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	status, code := "ok", http.StatusOK
	// If a relay is wired and its read loop has died, report the degradation
	// so an orchestrator restarts the container instead of trusting a zombie
	// that still hands out relay tokens for a relay that forwards nothing.
	if h, ok := s.relayAllocator.(interface{ Healthy() bool }); ok && !h.Healthy() {
		status, code = "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, HealthResponse{
		Status:        status,
		Version:       s.cfg.ServerVersion,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
	})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	clientIP := clientIP(r)
	rateKey := rateLimitKey(clientIP)

	// The slot is required and client-derived: the client generates the
	// code locally and sends only its argon2id stretch (internal/code.Slot).
	// The server never sees the code, so it can't run the PAKE against
	// either peer. Validate the shape so the session table only ever
	// holds well-formed slots.
	var body CreateSessionRequest
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json")
		return
	}
	slot := strings.ToLower(strings.TrimSpace(body.Slot))
	if code.ValidateSlot(slot) != nil {
		writeJSONError(w, http.StatusBadRequest, "missing or malformed slot")
		return
	}

	s.mu.Lock()
	if !s.allowNewSession(rateKey, time.Now()) {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "rate limit hit")
		return
	}
	if s.ipCounts[rateKey] >= s.cfg.MaxSessionsPerIP {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "too many concurrent sessions for this IP")
		return
	}

	// Taken slot → 409; the client owns code generation, so it retries
	// with a fresh code+slot. With ~2^45 codes an honest collision is
	// effectively impossible, so this is bounded-retry insurance, not a
	// hot path.
	if _, taken := s.bySlot[slot]; taken {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "slot already in use")
		return
	}

	sid := ulid.Make().String()
	senderTok := newRoleToken()
	sess := &session{
		ID:            sid,
		Slot:          slot,
		SenderAddr:    clientIP,
		SenderRateKey: rateKey,
		SenderICE:     newIceCreds(),
		SenderToken:   senderTok,
		State:         "waiting",
		CreatedAt:     time.Now(),
		LastSeen:      time.Now(),
		waiters:       make(chan struct{}),
	}
	s.bySlot[slot] = sess
	s.byID[sid] = sess
	s.ipCounts[rateKey]++
	s.mu.Unlock()
	s.cfg.Logger.Debug("session created", "slot", slot, "ip", clientIP)

	writeJSON(w, http.StatusOK, CreateSessionResponse{
		SessionID:        sid,
		YourObservedAddr: r.RemoteAddr,
		IceCredentials:   sess.SenderICE,
		TTLSeconds:       int(s.cfg.UnpairedTTL.Seconds()),
		ServerVersion:    s.cfg.ServerVersion,
		RoleToken:        senderTok,
	})
}

func (s *Server) joinSession(w http.ResponseWriter, r *http.Request) {
	slot := strings.ToLower(r.PathValue("slot"))
	clientIP := clientIP(r)
	rateKey := rateLimitKey(clientIP)

	s.mu.Lock()
	// Rate-limit symmetrically with createSession: a join is the same
	// shape of new-session activity from the server's perspective, and
	// without this an attacker could probe the code space (each guess
	// submitted as its derived slot) or churn joins against a known
	// slot at line rate.
	if !s.allowNewSession(rateKey, time.Now()) {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "rate limit hit")
		return
	}
	sess, ok := s.bySlot[slot]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "code not found")
		return
	}
	if sess.State != "waiting" {
		s.mu.Unlock()
		writeJSONError(w, http.StatusConflict, "session already paired")
		return
	}
	if s.ipCounts[rateKey] >= s.cfg.MaxSessionsPerIP {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "too many concurrent sessions for this IP")
		return
	}
	sess.ReceiverAddr = clientIP
	sess.ReceiverRateKey = rateKey
	sess.ReceiverICE = newIceCreds()
	sess.ReceiverToken = newRoleToken()
	sess.State = "paired"
	sess.PairedAt = time.Now()
	s.ipCounts[rateKey]++
	close(sess.waiters)
	resp := JoinSessionResponse{
		SessionID:          sess.ID,
		YourObservedAddr:   r.RemoteAddr,
		PeerObservedAddr:   sess.SenderAddr,
		PeerIceCredentials: sess.SenderICE,
		YourIceCredentials: sess.ReceiverICE,
		RoleToken:          sess.ReceiverToken,
	}
	s.mu.Unlock()
	s.cfg.Logger.Debug("session paired", "slot", slot, "ip", clientIP)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) waitSession(w http.ResponseWriter, r *http.Request) {
	slot := strings.ToLower(r.PathValue("slot"))
	s.mu.Lock()
	// Same rate limit as create/join. Without it, /wait is an
	// unthrottled existence oracle over the slot space (404 vs 204 for
	// the same lookup join performs). The legitimate sender polls once
	// per LongPollTimeout (~25s ≈ 2-3/min), far under the default
	// 30/min budget — see TestWaitRateLimit_PollCadenceStaysUnderBudget.
	if !s.allowNewSession(rateLimitKey(clientIP(r)), time.Now()) {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "rate limit hit")
		return
	}
	sess, ok := s.bySlot[slot]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "code not found")
		return
	}
	// Each /wait poll doubles as the sender's liveness heartbeat.
	sess.LastSeen = time.Now()
	// Snapshot what we need under the lock so concurrent joinSession
	// writes don't race the reads below.
	state := sess.State
	receiverAddr := sess.ReceiverAddr
	receiverICE := sess.ReceiverICE
	waiters := sess.waiters
	s.mu.Unlock()
	if state == "paired" {
		writeJSON(w, http.StatusOK, WaitResponse{
			PeerObservedAddr:   receiverAddr,
			PeerIceCredentials: receiverICE,
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.LongPollTimeout)
	defer cancel()
	select {
	case <-ctx.Done():
		w.WriteHeader(http.StatusNoContent)
	case <-waiters:
		// Re-fetch in case janitor evicted between wakeup and now.
		s.mu.Lock()
		final, ok := s.bySlot[slot]
		var finalAddr string
		var finalICE IceCreds
		if ok {
			finalAddr = final.ReceiverAddr
			finalICE = final.ReceiverICE
		}
		s.mu.Unlock()
		if !ok {
			writeJSONError(w, http.StatusGone, "session expired")
			return
		}
		writeJSON(w, http.StatusOK, WaitResponse{
			PeerObservedAddr:   finalAddr,
			PeerIceCredentials: finalICE,
		})
	}
}

func (s *Server) pushCandidates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body CandidatesPushRequest
	if err := decodeJSON(r, &body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "bad json")
		return
	}
	tok := bearerToken(r)
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	side, ok := sess.sideForToken(tok)
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusUnauthorized, "missing or invalid role token")
		return
	}
	target := &sess.SenderCandidates
	if side == "receiver" {
		target = &sess.ReceiverCandidates
	}
	// Cap the accumulated count per side. A legitimate peer never gathers
	// near maxCandidatesPerSide; a peer that does is either misbehaving
	// or compromised. Reject the whole batch rather than silently truncating
	// so the client surfaces the misconfiguration in --debug.
	if len(*target)+len(body.Candidates) > maxCandidatesPerSide {
		s.mu.Unlock()
		writeJSONError(w, http.StatusRequestEntityTooLarge, "candidate cap reached")
		return
	}
	*target = append(*target, body.Candidates...)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) pullCandidates(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	since := 0
	if v := r.URL.Query().Get("since"); v != "" {
		// Skip silently on parse error — clients that send garbage just
		// get the full candidate list starting at 0, which is harmless.
		_, _ = fmt.Sscanf(v, "%d", &since)
		if since < 0 {
			since = 0 // negative would panic the slice below
		}
	}
	tok := bearerToken(r)
	s.mu.Lock()
	sess, ok := s.byID[id]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	side, ok := sess.sideForToken(tok)
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusUnauthorized, "missing or invalid role token")
		return
	}
	var theirs []string
	if side == "sender" {
		theirs = sess.ReceiverCandidates
	} else {
		theirs = sess.SenderCandidates
	}
	s.mu.Unlock()

	if since < len(theirs) {
		writeJSON(w, http.StatusOK, CandidatesPullResponse{
			Candidates: theirs[since:],
			NextSince:  len(theirs),
		})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok := bearerToken(r)
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	// Gate on the role token like every other {id} endpoint: a session ID
	// alone (it travels over the pairing channel) must not let a third
	// party tear down someone else's session.
	if _, ok := sess.sideForToken(tok); !ok {
		writeJSONError(w, http.StatusUnauthorized, "missing or invalid role token")
		return
	}
	delete(s.byID, id)
	delete(s.bySlot, sess.Slot)
	s.releaseIP(sess.SenderRateKey)
	s.releaseIP(sess.ReceiverRateKey)
	s.cfg.Logger.Debug("session deleted", "slot", sess.Slot)
	w.WriteHeader(http.StatusNoContent)
}

// allowNewSession must be called with s.mu held. key is the
// rateLimitKey-collapsed identity (raw v4, /64 for v6).
func (s *Server) allowNewSession(key string, now time.Time) bool {
	b := s.ipBucket[key]
	if b == nil {
		b = &rateBucket{}
		s.ipBucket[key] = b
	}
	return b.allow(now, s.cfg.MaxNewSessionsPerMin, time.Minute)
}

// releaseIP must be called with s.mu held. key is the
// rateLimitKey-collapsed identity used to increment ipCounts at create
// or join — passing anything else would leak slots.
func (s *Server) releaseIP(key string) {
	if key == "" {
		return
	}
	s.ipCounts[key]--
	if s.ipCounts[key] <= 0 {
		delete(s.ipCounts, key)
	}
}

// iceCredEnc is Crockford-style base32 (no padding) used for ICE
// credential strings.
var iceCredEnc = base32.NewEncoding("ABCDEFGHJKMNPQRSTVWXYZ0123456789").
	WithPadding(base32.NoPadding)

// newRoleToken returns an opaque 128-bit bearer credential used to
// identify which side (sender/receiver) is making a candidate call.
// Replaces the older source-IP heuristic that broke whenever both
// peers shared a public IP (same NAT, corporate network, VPN exit).
func newRoleToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return iceCredEnc.EncodeToString(b[:])
}

// bearerToken extracts the bearer token from the Authorization header.
// Returns "" if the header is missing or malformed; callers treat that
// as "no token supplied" and respond with 401.
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}

// sideForToken identifies which peer a bearer token belongs to.
// crypto/subtle.ConstantTimeCompare keeps the auth check from leaking
// timing — it also handles the length-mismatch short-circuit safely.
func (s *session) sideForToken(tok string) (string, bool) {
	if tok == "" {
		return "", false
	}
	if s.SenderToken != "" && subtle.ConstantTimeCompare([]byte(s.SenderToken), []byte(tok)) == 1 {
		return "sender", true
	}
	if s.ReceiverToken != "" && subtle.ConstantTimeCompare([]byte(s.ReceiverToken), []byte(tok)) == 1 {
		return "receiver", true
	}
	return "", false
}

// newIceCreds returns a fresh ICE ufrag+pwd pair. ICE expects ufrag at
// least 4 chars and pwd at least 22 chars (RFC 5245 §15.4).
func newIceCreds() IceCreds {
	var u [4]byte
	var p [16]byte
	_, _ = rand.Read(u[:])
	_, _ = rand.Read(p[:])
	return IceCreds{
		Ufrag: iceCredEnc.EncodeToString(u[:]),
		Pwd:   iceCredEnc.EncodeToString(p[:]),
	}
}

// clientIP extracts the client's source IP, preferring the X-Real-IP
// header injected by Caddy when the server is behind a reverse proxy.
func clientIP(r *http.Request) string {
	if v := r.Header.Get("X-Real-IP"); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			return strings.TrimSpace(v[:i])
		}
		return strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// rateLimitKey collapses a client IP into the unit we want to throttle.
// IPv4 → the full /32. IPv6 → the /64 prefix.
//
// Why /64 for v6: every end host gets at least a /64 from its ISP (RFC
// 6177) and can rotate /128 addresses inside that prefix freely. Keying
// on the raw /128 lets a single hostile peer mint sessions without
// bound, while penalising no one legitimate; keying on the /64 matches
// the smallest prefix a normal network ever assigns to one customer.
//
// On unparseable input we return the original string — that keeps the
// existing behaviour for tests that pass arbitrary tokens as the "IP",
// and means a malformed X-Real-IP is still rate-limited (just on whatever
// raw bytes the peer sent).
func rateLimitKey(ip string) string {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return v4.String()
	}
	mask := net.CIDRMask(64, 128)
	return parsed.Mask(mask).String() + "/64"
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, ErrorResponse{Error: msg})
}

// decodeJSON reads at most maxRequestBodyBytes from r.Body and decodes
// it into v. Callers handle the error as a 400 — we don't try to
// distinguish "malformed JSON" from "body too large" because either way
// the right response is the same.
func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxRequestBodyBytes)).Decode(v)
}
