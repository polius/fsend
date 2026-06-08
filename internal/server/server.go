package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
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
	byCode   map[string]*session
	byID     map[string]*session
	ipCounts map[string]int         // active sessions per source IP
	ipBucket map[string]*rateBucket // new-session rate limiter per source IP

	// Relay-fallback wiring (optional). When non-nil, the server exposes
	// POST /v1/relay/allocate and returns RelayPublicAddr as the address
	// clients should send framed datagrams to.
	relayAllocator  RelayAllocator
	relayPublicAddr string
}

// RelayAllocator is the minimal interface internal/relay.Server exposes
// for our purposes — abstracting it lets us test the signaling layer
// without standing up real UDP sockets.
type RelayAllocator interface {
	Allocate() (relay.Token, error)
	Status(relay.Token) string
	Limits() (maxBytes uint64, idleTimeout time.Duration)
}

// WithRelay wires a relay allocator and its public address into the
// signaling layer. publicAddr is the host:port string clients dial.
func (s *Server) WithRelay(allocator RelayAllocator, publicAddr string) *Server {
	s.relayAllocator = allocator
	s.relayPublicAddr = publicAddr
	return s
}

// New constructs a Server with defaults filled in.
func New(cfg Config) *Server {
	cfg.Default()
	return &Server{
		cfg:      cfg,
		started:  time.Now(),
		byCode:   make(map[string]*session),
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
	m.HandleFunc("POST /v1/session/{code}/join", s.joinSession)
	m.HandleFunc("POST /v1/session/{code}/wait", s.waitSession)
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
	if !ok || !sess.relayTokenSet {
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
	maxBytes, idle := s.relayAllocator.Limits()
	switch reason {
	case relay.ReasonCapHit:
		resp.LimitBytes = maxBytes
	case relay.ReasonIdle:
		resp.IdleSeconds = int(idle / time.Second)
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
		RelayAddr:    s.relayPublicAddr,
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
			s.evict(now)
		}
	}
}

func (s *Server) evict(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.byID {
		switch sess.State {
		case "waiting":
			if now.Sub(sess.CreatedAt) > s.cfg.UnpairedTTL {
				delete(s.byID, id)
				delete(s.byCode, sess.Code)
				s.releaseIP(sess.SenderAddr)
				close(sess.waiters)
			}
		case "paired":
			if now.Sub(sess.PairedAt) > s.cfg.PairedTTL {
				delete(s.byID, id)
				delete(s.byCode, sess.Code)
				s.releaseIP(sess.SenderAddr)
				s.releaseIP(sess.ReceiverAddr)
			}
		}
	}
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		Status:        "ok",
		Version:       s.cfg.ServerVersion,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
	})
}

func (s *Server) createSession(w http.ResponseWriter, r *http.Request) {
	clientIP := clientIP(r)

	// Parse body up-front so we can honor a client-suggested code. An
	// empty/missing body is fine — old clients don't send one and we
	// fall back to server-side generation as before.
	var body CreateSessionRequest
	if r.Body != nil {
		_ = decodeJSON(r, &body) // bad json → treat as empty body
	}

	s.mu.Lock()
	if !s.allowNewSession(clientIP, time.Now()) {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "rate limit hit")
		return
	}
	if s.ipCounts[clientIP] >= s.cfg.MaxSessionsPerIP {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "too many concurrent sessions for this IP")
		return
	}

	// Adopt the client's code when it's valid and free; otherwise generate.
	// We do not 409 on a taken suggestion — the client has already shown
	// the code to the user, and falling back silently is preferable to
	// asking the user to re-share. The response carries the actual code so
	// the (rare) collision case is correct, just visibly different.
	c := strings.ToLower(strings.TrimSpace(body.Code))
	if c != "" && code.Validate(c) == nil {
		if _, taken := s.byCode[c]; taken {
			c = ""
		}
	} else {
		c = ""
	}
	if c == "" {
		var err error
		c, err = s.generateUniqueCode()
		if err != nil {
			s.mu.Unlock()
			writeJSONError(w, http.StatusInternalServerError, "code generation failed")
			return
		}
	}

	sid := ulid.Make().String()
	senderTok := newRoleToken()
	sess := &session{
		ID:          sid,
		Code:        c,
		SenderAddr:  clientIP,
		SenderICE:   newIceCreds(),
		SenderToken: senderTok,
		State:       "waiting",
		CreatedAt:   time.Now(),
		waiters:     make(chan struct{}),
	}
	s.byCode[c] = sess
	s.byID[sid] = sess
	s.ipCounts[clientIP]++
	s.mu.Unlock()

	writeJSON(w, http.StatusOK, CreateSessionResponse{
		SessionID:        sid,
		Code:             c,
		YourObservedAddr: r.RemoteAddr,
		IceCredentials:   sess.SenderICE,
		TTLSeconds:       int(s.cfg.UnpairedTTL.Seconds()),
		ServerVersion:    s.cfg.ServerVersion,
		RoleToken:        senderTok,
	})
}

func (s *Server) joinSession(w http.ResponseWriter, r *http.Request) {
	c := strings.ToLower(r.PathValue("code"))
	clientIP := clientIP(r)

	s.mu.Lock()
	// Rate-limit symmetrically with createSession: a join is the same
	// shape of new-session activity from the server's perspective, and
	// without this an attacker could probe the code space (or churn
	// joins against a known code) at line rate.
	if !s.allowNewSession(clientIP, time.Now()) {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "rate limit hit")
		return
	}
	sess, ok := s.byCode[c]
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
	if s.ipCounts[clientIP] >= s.cfg.MaxSessionsPerIP {
		s.mu.Unlock()
		writeJSONError(w, http.StatusTooManyRequests, "too many concurrent sessions for this IP")
		return
	}
	sess.ReceiverAddr = clientIP
	sess.ReceiverICE = newIceCreds()
	sess.ReceiverToken = newRoleToken()
	sess.State = "paired"
	sess.PairedAt = time.Now()
	s.ipCounts[clientIP]++
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
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) waitSession(w http.ResponseWriter, r *http.Request) {
	c := strings.ToLower(r.PathValue("code"))
	s.mu.Lock()
	sess, ok := s.byCode[c]
	if !ok {
		s.mu.Unlock()
		writeJSONError(w, http.StatusNotFound, "code not found")
		return
	}
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
		final, ok := s.byCode[c]
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
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.byID[id]
	if !ok {
		writeJSONError(w, http.StatusNotFound, "session not found")
		return
	}
	delete(s.byID, id)
	delete(s.byCode, sess.Code)
	s.releaseIP(sess.SenderAddr)
	s.releaseIP(sess.ReceiverAddr)
	w.WriteHeader(http.StatusNoContent)
}

// allowNewSession must be called with s.mu held.
func (s *Server) allowNewSession(ip string, now time.Time) bool {
	b := s.ipBucket[ip]
	if b == nil {
		b = &rateBucket{}
		s.ipBucket[ip] = b
	}
	return b.allow(now, s.cfg.MaxNewSessionsPerMin, time.Minute)
}

// releaseIP must be called with s.mu held.
func (s *Server) releaseIP(ip string) {
	if ip == "" {
		return
	}
	s.ipCounts[ip]--
	if s.ipCounts[ip] <= 0 {
		delete(s.ipCounts, ip)
	}
}

// generateUniqueCode picks a fresh code that isn't currently in use.
// Caller must hold s.mu.
func (s *Server) generateUniqueCode() (string, error) {
	for i := 0; i < 5; i++ {
		c, err := code.Generate()
		if err != nil {
			return "", err
		}
		if _, taken := s.byCode[c]; !taken {
			return c, nil
		}
	}
	return "", errors.New("could not pick a unique code")
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
