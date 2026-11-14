package iceconn

import (
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/ice/v4"

	"github.com/polius/fsend/internal/relay"
)

// icePacketConn adapts *ice.Conn (a stream-style net.Conn) into the
// datagram-style net.PacketConn that quic-go's Transport expects.
//
// This works because pion's selected-pair forwarding is datagram-
// preserving: each Write to ice.Conn produces one UDP datagram on the
// wire, and each Read returns one datagram's worth.
//
// Reads go through a read-ahead queue (audit N4). pion buffers what
// arrives on the selected pair in 1 MB (an unexported constant) and drops
// the rest. A QUIC window fills that in one burst whenever quic-go's read
// loop falls a little behind, and the losses collapse the congestion
// window: the direct route moved 3-9 MB/s on loopback. A goroutine that
// keeps draining pion into a queue sized like the kernel buffers quic-go
// asks for on its own sockets keeps pion's buffer near empty (52 MB/s).
type icePacketConn struct {
	c        *ice.Conn
	peerAddr net.Addr

	in chan *[]byte // datagrams read ahead of quic-go; closed when reads end

	mu   sync.Mutex
	err  error     // why reads ended, set before in closes
	dl   time.Time // read deadline
	wake chan struct{}
}

// readAhead bounds the queue: about 12 MB of full-size datagrams, near the
// 7 MB kernel buffers quic-go requests, counted in packets.
const readAhead = 8192

// datagramSize holds any datagram quic-go sends (at most 1,452 bytes).
const datagramSize = 2048

var datagramPool = sync.Pool{New: func() any {
	b := make([]byte, datagramSize)
	return &b
}}

func wrapAsPacket(c *ice.Conn) net.PacketConn {
	addr := c.RemoteAddr()
	if addr == nil {
		addr = relay.SyntheticPeer
	}
	p := &icePacketConn{
		c:        c,
		peerAddr: addr,
		in:       make(chan *[]byte, readAhead),
		wake:     make(chan struct{}, 1),
	}
	go p.drain()
	return p
}

// drain reads pion's conn as fast as datagrams arrive. When quic-go is so
// far behind that the queue is full, the datagram is dropped, as a full
// socket buffer would.
func (p *icePacketConn) drain() {
	for {
		bp := datagramPool.Get().(*[]byte)
		n, err := p.c.Read((*bp)[:datagramSize])
		if err != nil {
			datagramPool.Put(bp)
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
			close(p.in)
			return
		}
		*bp = (*bp)[:n]
		select {
		case p.in <- bp:
		default:
			datagramPool.Put(bp)
		}
	}
}

// ReadFrom returns the next datagram and reports the synthetic peer
// address. The address is stable across reads — quic-go uses it to key
// per-peer connection state.
func (p *icePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		p.mu.Lock()
		dl := p.dl
		p.mu.Unlock()
		var expire <-chan time.Time
		var t *time.Timer
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, nil, os.ErrDeadlineExceeded
			}
			t = time.NewTimer(d)
			expire = t.C
		}
		select {
		case bp, ok := <-p.in:
			if t != nil {
				t.Stop()
			}
			if !ok {
				p.mu.Lock()
				err := p.err
				p.mu.Unlock()
				return 0, nil, err
			}
			n := copy(b, *bp)
			*bp = (*bp)[:cap(*bp)]
			datagramPool.Put(bp)
			return n, p.peerAddr, nil
		case <-expire:
			return 0, nil, os.ErrDeadlineExceeded
		case <-p.wake: // the deadline changed
			if t != nil {
				t.Stop()
			}
		}
	}
}

// WriteTo ignores addr (ice.Conn already has a selected pair) and sends
// the payload over the ICE selected candidate pair.
func (p *icePacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	return p.c.Write(b)
}

func (p *icePacketConn) Close() error        { return p.c.Close() }
func (p *icePacketConn) LocalAddr() net.Addr { return p.c.LocalAddr() }

func (p *icePacketConn) SetDeadline(t time.Time) error {
	_ = p.SetReadDeadline(t)
	return p.c.SetWriteDeadline(t)
}

// SetReadDeadline applies to ReadFrom (the read-ahead queue), not to
// pion's conn, which the drainer keeps reading.
func (p *icePacketConn) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	p.dl = t
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
	return nil
}

func (p *icePacketConn) SetWriteDeadline(t time.Time) error { return p.c.SetWriteDeadline(t) }
