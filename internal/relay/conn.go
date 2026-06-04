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

	// peerAddr is what callers see as the source of inbound packets and
	// the destination of outbound ones. We surface a fake address rather
	// than the real relay so quic-go's per-peer state stays consistent.
	peerAddr net.Addr

	mu     sync.Mutex
	closed bool
}

// peerSyntheticAddr is the address we present to quic-go for the peer.
// Since we always WriteTo the relay (regardless of who we're "really"
// addressing), the address is purely a tag.
type peerSyntheticAddr string

func (a peerSyntheticAddr) Network() string { return "udp" }
func (a peerSyntheticAddr) String() string  { return string(a) }

// NewClient wraps an underlying UDP socket for relay-mode operation.
//
// The caller is expected to have already POST'd /v1/relay/allocate and
// gotten back the relayAddr and token. PeerLabel is any string to use
// as the synthetic peer address; both peers must agree (or both can pick
// independently — quic-go just needs a stable label).
func NewClient(underlying net.PacketConn, relayAddr *net.UDPAddr, token Token, peerLabel string) *Conn {
	return &Conn{
		underlying: underlying,
		relayAddr:  relayAddr,
		token:      token,
		peerAddr:   peerSyntheticAddr(peerLabel),
	}
}

// LocalAddr returns the underlying socket's local address.
func (c *Conn) LocalAddr() net.Addr { return c.underlying.LocalAddr() }

// PeerAddr returns the synthetic peer address.
func (c *Conn) PeerAddr() net.Addr { return c.peerAddr }

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
		return n, c.peerAddr, nil
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
