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
)

// Server is the UDP-relay half of fsend-server.
//
// It owns a single net.PacketConn (the UDP/443 listener). For each
// allocation, it tracks the two peers' addresses (set on first datagram
// from each) and forwards traffic between them. See
// docs/decisions/relay-protocol.md for the design rationale.
type Server struct {
	conn   net.PacketConn
	cfg    ServerConfig
	logger *slog.Logger

	mu        sync.RWMutex
	allocs    map[Token]*allocation
	ipBytes   map[string]uint64 // for ipBytes-per-cap aggregation if needed later
}

// ServerConfig holds the per-session/per-ip tuning.
type ServerConfig struct {
	MaxBytesPerSession uint64
	SessionIdleTimeout time.Duration
	JanitorInterval    time.Duration
	Logger             *slog.Logger
}

// Default fills in zero values.
func (c *ServerConfig) Default() {
	if c.MaxBytesPerSession == 0 {
		c.MaxBytesPerSession = 100 * 1024 * 1024 // 100 MiB
	}
	if c.SessionIdleTimeout == 0 {
		c.SessionIdleTimeout = 60 * time.Second
	}
	if c.JanitorInterval == 0 {
		c.JanitorInterval = 30 * time.Second
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
	return &Server{
		conn:    conn,
		cfg:     cfg,
		logger:  cfg.Logger,
		allocs:  make(map[Token]*allocation),
		ipBytes: make(map[string]uint64),
	}
}

// Allocate creates a new relay slot and returns the token clients embed
// in their datagrams. Called from the signaling layer's
// POST /v1/relay/allocate handler.
func (s *Server) Allocate() (Token, error) {
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
			// Other read errors (e.g. socket closed): bail out.
			return fmt.Errorf("relay: read: %w", err)
		}
		// Process inline — no goroutine per datagram (would be massive
		// overhead). Forward is one syscall.
		s.handle(buf[:n], addr.(*net.UDPAddr))
		bufPool.Put(bufPtr)
	}
}

func (s *Server) handle(datagram []byte, src *net.UDPAddr) {
	ver, token, payload, ok := Parse(datagram)
	if !ok {
		return // silently drop malformed
	}
	if ver != ProtocolVersion {
		return
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
		s.evict(token, "byte cap reached")
		return
	}

	if _, err := s.conn.WriteTo(datagram, dst); err != nil {
		s.logger.Debug("relay: write failed", "err", err)
	}
	_ = payload // referenced for any future per-payload accounting
}

func (s *Server) evict(t Token, reason string) {
	s.mu.Lock()
	delete(s.allocs, t)
	s.mu.Unlock()
	s.logger.Debug("relay: evicted", "token", t.String(), "reason", reason)
}

func (s *Server) janitor(ctx context.Context) {
	t := time.NewTicker(s.cfg.JanitorInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			cutoff := now.Add(-s.cfg.SessionIdleTimeout).UnixNano()
			s.mu.Lock()
			for tok, a := range s.allocs {
				if a.lastActivity.Load() < cutoff {
					delete(s.allocs, tok)
				}
			}
			s.mu.Unlock()
		}
	}
}

// udpEqual reports whether two UDPAddrs refer to the same endpoint.
func udpEqual(a, b *net.UDPAddr) bool {
	return a != nil && b != nil && a.Port == b.Port && a.IP.Equal(b.IP)
}
