package relay

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Conn is a client-side net.PacketConn that frames every outbound datagram
// with the relay header and unwraps inbound ones.
//
// It satisfies net.PacketConn, which is the interface quic-go's Transport
// accepts via Transport.Conn. That means we can run QUIC end-to-end
// between two peers, with all packets transparently relayed through
// the server — the relay only sees opaque ciphertext after the QUIC
// handshake completes.
//
// Concurrency: ReadFrom / WriteTo / Close may all be called from
// different goroutines (quic-go does exactly that).
type Conn struct {
	underlying net.PacketConn
	relayAddr  *net.UDPAddr
	token      Token

	gotInbound atomic.Bool   // set once the relay forwards us a datagram
	stop       chan struct{} // closed by Close to stop the bootstrap resender
	stopOnce   sync.Once

	mu     sync.Mutex
	closed bool
}

// syntheticPeer is the address we surface to quic-go as the remote peer.
// We always WriteTo the relay regardless of destination, so the address
// is purely a routing tag — a stable synthetic UDPAddr suffices.
var syntheticPeer = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1}

// NewClient wraps an underlying UDP socket for relay-mode operation.
//
// The caller is expected to have already POST'd /v1/relay/allocate and
// gotten back the relayAddr and token.
func NewClient(underlying net.PacketConn, relayAddr *net.UDPAddr, token Token) *Conn {
	return &Conn{
		underlying: underlying,
		relayAddr:  relayAddr,
		token:      token,
		stop:       make(chan struct{}),
	}
}

// KeepBootstrapping resends the one-byte bootstrap datagram every interval
// (up to max) until the relay forwards us our first datagram, or the conn
// closes. The bootstrap is how the relay learns our address; after it, the
// sender is passive (it waits to be dialed), so a single lost bootstrap
// would strand the pairing. Harmless for the receiver — it dials QUIC
// immediately, so its first inbound arrives fast and the resender stops.
func (c *Conn) KeepBootstrapping(interval, max time.Duration) {
	go func() {
		deadline := time.Now().Add(max)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-c.stop:
				return
			case <-t.C:
				if c.gotInbound.Load() || time.Now().After(deadline) {
					return
				}
				_, _ = c.WriteTo([]byte{0}, nil)
			}
		}
	}()
}

// LocalAddr returns the underlying socket's local address.
func (c *Conn) LocalAddr() net.Addr { return c.underlying.LocalAddr() }

// SetDeadline plumbs through to the underlying socket.
func (c *Conn) SetDeadline(t time.Time) error { return c.underlying.SetDeadline(t) }

// SetReadDeadline plumbs through to the underlying socket.
func (c *Conn) SetReadDeadline(t time.Time) error { return c.underlying.SetReadDeadline(t) }

// SetWriteDeadline plumbs through to the underlying socket.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.underlying.SetWriteDeadline(t) }

// Close shuts the underlying socket. Idempotent.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.stopOnce.Do(func() { close(c.stop) })
	return c.underlying.Close()
}

// ReadFrom blocks until a framed datagram arrives, then unwraps it and
// returns the inner payload + the synthetic peer address.
//
// Datagrams whose token doesn't match ours are dropped silently (defense
// in depth: should not happen given the relay's demux, but cheap to check).
func (c *Conn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	buf := make([]byte, len(p)+HeaderSize)
	for {
		nn, _, err := c.underlying.ReadFrom(buf)
		if err != nil {
			return 0, nil, err
		}
		ver, token, payload, ok := Parse(buf[:nn])
		if !ok || ver != ProtocolVersion {
			continue
		}
		if token != c.token {
			continue
		}
		// The relay only forwards once both peers are registered, so any
		// inbound datagram means our address is known — stop resending.
		c.gotInbound.Store(true)
		n = copy(p, payload)
		return n, syntheticPeer, nil
	}
}

// WriteTo frames the payload and forwards it to the relay.
//
// The `addr` argument is ignored — we always write to the relay. This
// matches quic-go's expectation that PacketConn semantics are stable.
func (c *Conn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if len(p)+HeaderSize > 65535 {
		return 0, fmt.Errorf("relay: payload too large")
	}
	out := make([]byte, HeaderSize+len(p))
	Frame(out, c.token, p)
	n, err := c.underlying.WriteTo(out, c.relayAddr)
	if err != nil {
		return 0, err
	}
	// Report the original (unframed) length so callers see what they sent.
	if n < HeaderSize {
		return 0, errors.New("relay: short write")
	}
	return n - HeaderSize, nil
}
