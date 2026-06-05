package relay

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"
)

// Conn is a client-side net.PacketConn that frames every outbound datagram
// with the relay header and unwraps inbound ones.
//
// It satisfies net.PacketConn, which is the interface quic-go's Transport
// accepts via Transport.Conn. That means we can run QUIC end-to-end
// between two peers, with all packets transparently relayed through
// fsend-server — the relay only sees opaque ciphertext after the QUIC
// handshake completes.
//
// Concurrency: ReadFrom / WriteTo / Close may all be called from
// different goroutines (quic-go does exactly that).
type Conn struct {
	underlying net.PacketConn
	relayAddr  *net.UDPAddr
	token      Token

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
	}
}

// LocalAddr returns the underlying socket's local address.
func (c *Conn) LocalAddr() net.Addr { return c.underlying.LocalAddr() }

// SetDeadline plumbs through to the underlying socket.
func (c *Conn) SetDeadline(t time.Time) error      { return c.underlying.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.underlying.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.underlying.SetWriteDeadline(t) }

// Close shuts the underlying socket. Idempotent.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
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
