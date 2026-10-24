package relay

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/stun/v4"
)

// DialWS opens the relay's TCP fallback: a WebSocket to wsURL (the
// server's /v1/relay/ws, authenticated by header) carrying the same frames
// as UDP. It returns a registered Conn, ready for QUIC. ctx bounds the
// dial and registration only.
func DialWS(ctx context.Context, wsURL string, header http.Header, tok Token, id PeerID) (*Conn, error) {
	ws, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, wsDialErr(resp, err)
	}
	ws.SetReadLimit(MaxDatagram)
	pc := newWSPacketConn(ws, wsURL)
	c := NewClient(pc, pc.addr, tok).WithPeerID(id)
	if err := c.Register(ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// ProbeWS checks the relay's TCP fallback end to end: it opens a WebSocket
// to wsURL, has the server echo a message, and returns the response
// headers (the server names its UDP relay address there).
func ProbeWS(ctx context.Context, wsURL string, header http.Header) (http.Header, error) {
	ws, resp, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, wsDialErr(resp, err)
	}
	defer func() { _ = ws.CloseNow() }()
	ping := make([]byte, 16)
	_, _ = rand.Read(ping)
	if err := ws.Write(ctx, websocket.MessageBinary, ping); err != nil {
		return nil, err
	}
	_, echo, err := ws.Read(ctx)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(echo, ping) {
		return nil, errors.New("relay TCP fallback: echo mismatch")
	}
	_ = ws.Close(websocket.StatusNormalClosure, "")
	return resp.Header, nil
}

// ProbeSTUN sends STUN binding requests to the relay's UDP address until
// one is answered or ctx ends, and returns our public address as the
// relay saw it. No answer means UDP to the relay is blocked.
func ProbeSTUN(ctx context.Context, relayAddr string) (*net.UDPAddr, error) {
	raddr, err := net.ResolveUDPAddr("udp", relayAddr)
	if err != nil {
		return nil, err
	}
	pc, err := net.ListenUDP("udp", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = pc.Close() }()
	req, err := stun.Build(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	for {
		if _, err := pc.WriteTo(req.Raw, raddr); err != nil {
			return nil, err
		}
		dl := time.Now().Add(500 * time.Millisecond)
		if d, ok := ctx.Deadline(); ok && d.Before(dl) {
			dl = d
		}
		_ = pc.SetReadDeadline(dl)
		for {
			n, _, err := pc.ReadFrom(buf)
			if err != nil {
				break
			}
			resp := &stun.Message{Raw: append([]byte(nil), buf[:n]...)}
			if resp.Decode() != nil || resp.TransactionID != req.TransactionID {
				continue
			}
			var mapped stun.XORMappedAddress
			if err := mapped.GetFrom(resp); err != nil {
				return nil, err
			}
			return &net.UDPAddr{IP: mapped.IP, Port: mapped.Port}, nil
		}
		if ctx.Err() != nil {
			return nil, ErrNoReply
		}
	}
}

// HTTPStatusError is a WebSocket upgrade the server refused with an HTTP
// status (404: a server without the TCP fallback; 401: wrong password).
type HTTPStatusError struct{ Code int }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("relay TCP fallback: server answered HTTP %d", e.Code)
}

func wsDialErr(resp *http.Response, err error) error {
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		return &HTTPStatusError{Code: resp.StatusCode}
	}
	return fmt.Errorf("relay TCP fallback: %w", err)
}

// wsAddr names the WebSocket endpoint where a net.Addr is required.
type wsAddr string

func (a wsAddr) Network() string { return "websocket" }
func (a wsAddr) String() string  { return string(a) }

// wsPacketConn adapts a WebSocket to net.PacketConn: one binary message per
// datagram. A background reader feeds ReadFrom, so read deadlines (which
// quic-go uses to stop its read loop) never cancel the WebSocket itself.
type wsPacketConn struct {
	ws   *websocket.Conn
	addr wsAddr
	in   chan []byte

	ctx    context.Context // ends the link
	cancel context.CancelFunc
	dead   chan struct{} // closed when the reader stops
	err    error         // why the reader stopped; read after dead

	dlMu     sync.Mutex
	deadline time.Time
	wake     chan struct{} // closed when the deadline changes
}

func newWSPacketConn(ws *websocket.Conn, wsURL string) *wsPacketConn {
	addr := wsAddr(wsURL)
	if u, err := url.Parse(wsURL); err == nil {
		addr = wsAddr(u.Host)
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &wsPacketConn{
		ws:     ws,
		addr:   addr,
		in:     make(chan []byte, wsQueue),
		ctx:    ctx,
		cancel: cancel,
		dead:   make(chan struct{}),
		wake:   make(chan struct{}),
	}
	go p.readLoop()
	return p
}

func (p *wsPacketConn) readLoop() {
	defer close(p.dead)
	for {
		typ, msg, err := p.ws.Read(p.ctx)
		if err != nil {
			p.err = err
			return
		}
		if typ != websocket.MessageBinary {
			continue
		}
		select {
		case p.in <- msg:
		case <-p.ctx.Done():
			p.err = net.ErrClosed
			return
		}
	}
}

func (p *wsPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		if msg, again, err := p.next(); !again {
			if err != nil {
				return 0, nil, err
			}
			return copy(b, msg), p.addr, nil
		}
	}
}

// next waits for a message under the current read deadline; again means
// the deadline changed mid-wait and must be re-read.
func (p *wsPacketConn) next() (msg []byte, again bool, err error) {
	p.dlMu.Lock()
	dl, wake := p.deadline, p.wake
	p.dlMu.Unlock()
	var expired <-chan time.Time
	if !dl.IsZero() {
		d := time.Until(dl)
		if d <= 0 {
			return nil, false, os.ErrDeadlineExceeded
		}
		t := time.NewTimer(d)
		defer t.Stop()
		expired = t.C
	}
	select {
	case msg := <-p.in:
		return msg, false, nil
	case <-expired:
		return nil, false, os.ErrDeadlineExceeded
	case <-wake:
		return nil, true, nil
	case <-p.dead:
		if p.ctx.Err() != nil {
			return nil, false, net.ErrClosed
		}
		return nil, false, p.err
	}
}

func (p *wsPacketConn) WriteTo(b []byte, _ net.Addr) (int, error) {
	if err := p.ws.Write(p.ctx, websocket.MessageBinary, b); err != nil {
		if p.ctx.Err() != nil {
			return 0, net.ErrClosed
		}
		return 0, err
	}
	return len(b), nil
}

func (p *wsPacketConn) Close() error {
	p.cancel()
	_ = p.ws.CloseNow()
	return nil
}

func (p *wsPacketConn) LocalAddr() net.Addr { return p.addr }

func (p *wsPacketConn) SetDeadline(t time.Time) error { return p.SetReadDeadline(t) }

func (p *wsPacketConn) SetReadDeadline(t time.Time) error {
	p.dlMu.Lock()
	p.deadline = t
	close(p.wake)
	p.wake = make(chan struct{})
	p.dlMu.Unlock()
	return nil
}

// SetWriteDeadline is a no-op: writes block only on TCP backpressure,
// which is the pacing we want.
func (p *wsPacketConn) SetWriteDeadline(time.Time) error { return nil }
