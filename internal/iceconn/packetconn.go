package iceconn

import (
	"net"
	"time"

	"github.com/pion/ice/v4"

	"github.com/polius/fsend/internal/relay"
)

// icePacketConn adapts *ice.Conn (a stream-style net.Conn) into the
// datagram-style net.PacketConn that quic-go's Transport expects.
//
// This works because pion's selected-pair forwarding is datagram-
// preserving: each Write to ice.Conn produces one UDP datagram on the
// wire, and each Read returns one datagram's worth. We just need to
// satisfy the PacketConn interface — there is no buffering or
// fragmentation to worry about.
type icePacketConn struct {
	c        *ice.Conn
	peerAddr net.Addr
}

func wrapAsPacket(c *ice.Conn) net.PacketConn {
	addr := c.RemoteAddr()
	if addr == nil {
		addr = relay.SyntheticPeer
	}
	return &icePacketConn{c: c, peerAddr: addr}
}

// ReadFrom returns the next datagram and reports the synthetic peer
// address. The address is stable across reads — quic-go uses it to key
// per-peer connection state.
func (p *icePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.c.Read(b)
	if err != nil {
		return n, nil, err
	}
	return n, p.peerAddr, nil
}

// WriteTo ignores addr (ice.Conn already has a selected pair) and sends
// the payload over the ICE selected candidate pair.
func (p *icePacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return p.c.Write(b)
}

func (p *icePacketConn) Close() error                       { return p.c.Close() }
func (p *icePacketConn) LocalAddr() net.Addr                { return p.c.LocalAddr() }
func (p *icePacketConn) SetDeadline(t time.Time) error      { return p.c.SetDeadline(t) }
func (p *icePacketConn) SetReadDeadline(t time.Time) error  { return p.c.SetReadDeadline(t) }
func (p *icePacketConn) SetWriteDeadline(t time.Time) error { return p.c.SetWriteDeadline(t) }
