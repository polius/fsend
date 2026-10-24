package relay

import (
	"context"
	"crypto/rand"
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
// handshake completes. The underlying conn is a UDP socket aimed at the
// relay, or a WebSocket link (see DialWS); the framing is the same.
//
// Concurrency: ReadFrom / WriteTo / Close may all be called from
// different goroutines (quic-go does exactly that).
type Conn struct {
	underlying net.PacketConn
	relayAddr  net.Addr
	token      Token
	id         PeerID

	gotInbound atomic.Bool   // set once the relay forwards us a datagram
	stop       chan struct{} // closed by Close to stop the keepalive
	stopOnce   sync.Once

	readMu  sync.Mutex
	readBuf []byte

	mu     sync.Mutex
	closed bool
}

// SyntheticPeer is the address we surface to quic-go as the remote peer.
// We always route to the real destination through a fixed underlying path
// (the relay, or the ICE conn), so the address is purely a routing tag — a
// stable synthetic UDPAddr suffices. Shared by internal/iceconn and the CLI
// so both hand quic-go the same tag.
var SyntheticPeer = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 1}

// ErrNoReply means the relay never answered the registration handshake.
// Over UDP that is the signature of a network that blocks UDP.
var ErrNoReply = errors.New("relay: no reply")

// NewClient wraps an underlying socket for relay-mode operation, with a
// random PeerID (see WithPeerID). Call Register before handing it to QUIC.
//
// The caller is expected to have already POST'd /v1/relay/allocate and
// gotten back the relayAddr and token.
func NewClient(underlying net.PacketConn, relayAddr net.Addr, token Token) *Conn {
	c := &Conn{
		underlying: underlying,
		relayAddr:  relayAddr,
		token:      token,
		stop:       make(chan struct{}),
		readBuf:    make([]byte, MaxDatagram+1),
	}
	_, _ = rand.Read(c.id[:])
	return c
}

// WithPeerID sets the id the relay files this side under. A stable id
// (PeerIDFor) lets a re-dial reclaim this side's slot. Returns c.
func (c *Conn) WithPeerID(id PeerID) *Conn {
	c.id = id
	return c
}

// registerResend is how often Register repeats an unanswered message.
const registerResend = 250 * time.Millisecond

// keepaliveInterval refreshes our registration (and any NAT mapping on the
// way) while we wait, passively, for the first datagram from the peer.
const keepaliveInterval = 15 * time.Second

// Register runs the relay's registration handshake (HELLO → CHALLENGE →
// REGISTER → REGISTERED) and returns once the relay has recorded our
// address. It returns ErrNoReply if the relay never answered before ctx
// ended, so ctx should carry a deadline. Must be called before the conn is
// handed to QUIC: it reads the underlying conn directly.
func (c *Conn) Register(ctx context.Context) error {
	defer func() { _ = c.underlying.SetReadDeadline(time.Time{}) }()
	msg := c.control(msgHello, nil)
	buf := make([]byte, MaxDatagram)
	send := true
	for {
		if send {
			if _, err := c.underlying.WriteTo(msg, c.relayAddr); err != nil {
				return err
			}
			dl := time.Now().Add(registerResend)
			if d, ok := ctx.Deadline(); ok && d.Before(dl) {
				dl = d
			}
			_ = c.underlying.SetReadDeadline(dl)
			send = false
		}
		n, _, err := c.underlying.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if !errors.As(err, &ne) || !ne.Timeout() {
				return err
			}
			if ctx.Err() != nil {
				if errors.Is(ctx.Err(), context.Canceled) {
					return ctx.Err()
				}
				return ErrNoReply
			}
			send = true
			continue
		}
		tok, typ, id, cookie, ok := parseControl(buf[:n])
		if !ok || tok != c.token {
			continue
		}
		switch {
		case typ == msgChallenge:
			msg = c.control(msgRegister, &cookie)
			send = true
		case typ == msgRegistered && id == c.id:
			go c.keepalive()
			return nil
		}
	}
}

// keepalive repeats HELLO until the peer's first datagram arrives or the
// conn closes. The relay's CHALLENGE is answered in ReadFrom.
func (c *Conn) keepalive() {
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-t.C:
			if c.gotInbound.Load() {
				return
			}
			_, _ = c.underlying.WriteTo(c.control(msgHello, nil), c.relayAddr)
		}
	}
}

func (c *Conn) control(typ byte, cookie *[cookieSize]byte) []byte {
	return frameControl(c.token, typ, c.id, cookie)
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
// A CHALLENGE means the relay saw our data from an address it doesn't
// know (NAT rebinding, network change): we re-register from the new one.
func (c *Conn) ReadFrom(p []byte) (n int, addr net.Addr, err error) {
	c.readMu.Lock()
	defer c.readMu.Unlock()
	buf := c.readBuf
	for {
		nn, _, err := c.underlying.ReadFrom(buf)
		if err != nil {
			return 0, nil, err
		}
		if nn > 0 && buf[0] == ControlVersion {
			if tok, typ, _, cookie, ok := parseControl(buf[:nn]); ok && tok == c.token && typ == msgChallenge {
				_, _ = c.underlying.WriteTo(c.control(msgRegister, &cookie), c.relayAddr)
			}
			continue
		}
		ver, token, payload, ok := Parse(buf[:nn])
		if !ok || ver != ProtocolVersion || token != c.token {
			continue
		}
		if nn == len(buf) || len(payload) > len(p) {
			continue // oversize: drop rather than truncate
		}
		c.gotInbound.Store(true)
		n = copy(p, payload)
		return n, SyntheticPeer, nil
	}
}

// WriteTo frames the payload and forwards it to the relay.
//
// The `addr` argument is ignored — we always write to the relay. This
// matches quic-go's expectation that PacketConn semantics are stable.
func (c *Conn) WriteTo(p []byte, _ net.Addr) (int, error) {
	if len(p)+HeaderSize > MaxDatagram {
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
