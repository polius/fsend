package relay

import (
	"bytes"
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"
)

// The TCP fallback: for networks that block UDP, a peer attaches to its
// allocation over a WebSocket on the server's HTTPS port (behind the same
// TLS terminator as signaling). Each binary message is one relay frame,
// exactly as it would travel over UDP, so the two kinds of peer pair
// freely in one allocation.

// wsQueue bounds the frames waiting to go out on one link. Past it, frames
// drop like datagrams off a full socket buffer, and QUIC backs off.
const wsQueue = 256

// wsPingInterval keeps an idle link alive through proxies (deploy/compose's
// Caddy cuts a backend read after 30 s) and detects a dead TCP peer.
const wsPingInterval = 15 * time.Second

// wsLink is one WebSocket peer's outbound side.
type wsLink struct {
	out  chan []byte
	done chan struct{} // closed when the link ends
}

// send queues a copy of b (the caller's buffer is reused), dropping it if
// the link is backed up or gone.
func (l *wsLink) send(b []byte) {
	m := make([]byte, len(b))
	copy(m, b)
	select {
	case <-l.done:
	case l.out <- m:
	default:
	}
}

func (l *wsLink) writeLoop(ctx context.Context, c *websocket.Conn) {
	t := time.NewTicker(wsPingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.done:
			return
		case b := <-l.out:
			if c.Write(ctx, websocket.MessageBinary, b) != nil {
				_ = c.CloseNow()
				return
			}
		case <-t.C:
			go func() {
				pctx, cancel := context.WithTimeout(ctx, wsPingInterval)
				defer cancel()
				if c.Ping(pctx) != nil {
					_ = c.CloseNow()
				}
			}()
		}
	}
}

// ServeWS upgrades r to a WebSocket carrying relay frames for allocation
// tok. The caller must already have authenticated r as a party to the
// session. Blocks until the link ends: the peer leaves, the allocation is
// evicted, or Run returns.
func (s *Server) ServeWS(w http.ResponseWriter, r *http.Request, tok Token) {
	if s.cfg.DisableForwarding {
		http.Error(w, "relay forwarding disabled", http.StatusServiceUnavailable)
		return
	}
	if s.lookup(tok) == nil {
		http.Error(w, "relay allocation not found", http.StatusGone)
		return
	}
	c, err := acceptWS(w, r)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	s.tcpFallbacks.Add(1)
	c.SetReadLimit(MaxDatagram)

	ctx, cancel := context.WithCancel(s.life)
	defer cancel()
	l := &wsLink{out: make(chan []byte, wsQueue), done: make(chan struct{})}
	defer close(l.done)
	go l.writeLoop(ctx, c)

	for {
		typ, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		if typ != websocket.MessageBinary || len(msg) < HeaderSize || !bytes.Equal(msg[1:HeaderSize], tok[:]) {
			continue
		}
		if s.lookup(tok) == nil {
			return // evicted (cap, budget, idle): end the link
		}
		s.handleSafe(msg, endpoint{ws: l})
	}
}

// ProbeWS answers `fsend doctor`: upgrade, echo one small message, close.
// It shows the TCP fallback reaches this server through whatever proxies
// sit in between, without touching an allocation.
func (s *Server) ProbeWS(w http.ResponseWriter, r *http.Request) {
	if s.cfg.DisableForwarding {
		http.Error(w, "relay forwarding disabled", http.StatusServiceUnavailable)
		return
	}
	c, err := acceptWS(w, r)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(ControlSize)
	ctx, cancel := context.WithTimeout(s.life, 5*time.Second)
	defer cancel()
	typ, msg, err := c.Read(ctx)
	if err != nil || c.Write(ctx, typ, msg) != nil {
		return
	}
	_ = c.Close(websocket.StatusNormalClosure, "")
}

func acceptWS(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	// http.Server's ReadTimeout deadline survives the hijack; a long-lived
	// link must not inherit it.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	return websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
}
