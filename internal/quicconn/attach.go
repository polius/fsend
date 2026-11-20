package quicconn

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
)

// ALPNStripe is ALPN with striping: a peer that offers it splits the data
// stream into stripes (stripe.go) and opens extra lanes. Both peers know
// from the TLS handshake, before anything else is sent, so a peer that
// doesn't offer it (up to 1.17) is served exactly as before.
const ALPNStripe = "fsend/1+stripe"

// ALPNAttach marks an extra lane: a connection that joins a session
// already authenticated on its first connection, with a key derived from
// that session (no code exchange: nothing to guess, and no PAKE attempt
// spent).
const ALPNAttach = "fsend-attach/1"

// stripeLanes is how many connections a striped transfer uses, the first
// included. Four lift a path with 0.1% loss about 3.5x; past the link's
// capacity more only add per-connection overhead.
const stripeLanes = 4

// maxLanes bounds the lanes one session accepts.
const maxLanes = 16

// laneCount is stripeLanes, or FSEND_STRIPE_LANES (an undocumented test
// and benchmark knob; 1 keeps the striped framing on a single lane).
func laneCount() int {
	if v, err := strconv.Atoi(os.Getenv("FSEND_STRIPE_LANES")); err == nil && v >= 1 && v <= maxLanes {
		return v
	}
	return stripeLanes
}

// Labels for what the session binding derives for lanes.
const (
	labelLaneKey      = "fsend-lane-key-v1"
	labelLaneID       = "fsend-lane-id-v1"
	dirLaneReceiver   = "fsend-lane-receiver"
	dirLaneSender     = "fsend-lane-sender"
	attachMagic       = 'A'
	attachVersion     = 1
	laneIDLen         = 16
	attachMsgLen      = 2 + laneIDLen + 1 + tagLen
	closeLaneRejected = closeHandshakeFailed
)

// laneSecrets derives, from the session binding both peers hold after the
// PAKE, the key lanes authenticate with and the id the sender files the
// session under. The id reveals nothing about the key.
func laneSecrets(binding []byte) (key []byte, id [laneIDLen]byte) {
	key = hmacSum(binding, labelLaneKey)
	copy(id[:], hmacSum(binding, labelLaneID))
	return key, id
}

func hmacSum(key []byte, parts ...string) []byte {
	mac := hmac.New(sha256.New, key)
	for _, p := range parts {
		mac.Write([]byte(p))
	}
	return mac.Sum(nil)
}

// laneTag proves knowledge of the lane key on this TLS session (the
// exporter: a relay terminating TLS on each side sees two exporters, so a
// tag can't be carried from one to the other) for this lane and direction.
func laneTag(key []byte, dir string, exporter []byte, lane byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(dir))
	mac.Write(exporter)
	mac.Write([]byte{lane})
	return mac.Sum(nil)
}

func exporterOf(c *quic.Conn) ([]byte, error) {
	state := c.ConnectionState().TLS
	return state.ExportKeyingMaterial(exporterLabel, nil, exporterLen)
}

func striped(c *quic.Conn) bool { return c.ConnectionState().TLS.NegotiatedProtocol == ALPNStripe }

// laneSession is a sender session open to extra lanes.
type laneSession struct {
	key     []byte
	out     *stripeOut
	primary *quic.Conn
	lanes   int
}

func (l *Listener) addLaneSession(id [laneIDLen]byte, s *laneSession) {
	l.mu.Lock()
	if l.laneSessions == nil {
		l.laneSessions = make(map[[laneIDLen]byte]*laneSession)
	}
	l.laneSessions[id] = s
	l.mu.Unlock()
}

func (l *Listener) dropLaneSession(id [laneIDLen]byte) {
	l.mu.Lock()
	delete(l.laneSessions, id)
	l.mu.Unlock()
}

// laneSession returns the session id names, if it takes another lane.
func (l *Listener) laneSession(id [laneIDLen]byte) *laneSession {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.laneSessions[id]
	if s == nil || s.lanes >= maxLanes {
		return nil
	}
	return s
}

// claimLane counts a verified lane against its session's bound.
func (l *Listener) claimLane(s *laneSession) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s.lanes >= maxLanes {
		return false
	}
	s.lanes++
	return true
}

// acceptLane authenticates an extra lane and hands its data stream to the
// session's striped writer.
func (l *Listener) acceptLane(c *quic.Conn) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), HandshakeTimeout)
	defer cancel()
	defer closeOnFailure(c, &err)
	st, err := c.AcceptStream(ctx)
	if err != nil {
		return fmt.Errorf("quicconn: lane: accept: %w", err)
	}
	_ = st.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	var msg [attachMsgLen]byte
	if _, err := io.ReadFull(st, msg[:]); err != nil {
		return fmt.Errorf("quicconn: lane: read: %w", err)
	}
	if msg[0] != attachMagic || msg[1] != attachVersion {
		return fmt.Errorf("%w: lane: bad attach message", fserrors.ErrPeerAuthFailed)
	}
	var id [laneIDLen]byte
	copy(id[:], msg[2:2+laneIDLen])
	lane, tag := msg[2+laneIDLen], msg[3+laneIDLen:]
	s := l.laneSession(id)
	if s == nil {
		return fmt.Errorf("%w: lane: no such session", fserrors.ErrPeerAuthFailed)
	}
	exporter, err := exporterOf(c)
	if err != nil {
		return fmt.Errorf("quicconn: lane: exporter: %w", err)
	}
	if !hmac.Equal(tag, laneTag(s.key, dirLaneReceiver, exporter, lane)) {
		return fmt.Errorf("%w: lane: wrong tag", fserrors.ErrPeerAuthFailed)
	}
	if !l.claimLane(s) {
		return fmt.Errorf("%w: lane: session has its lanes", fserrors.ErrPeerAuthFailed)
	}
	if _, err := st.Write(laneTag(s.key, dirLaneSender, exporter, lane)); err != nil {
		return fmt.Errorf("quicconn: lane: write: %w", err)
	}
	data, err := c.OpenUniStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("quicconn: lane: open data: %w", err)
	}
	if _, err := data.Write([]byte{0}); err != nil {
		return fmt.Errorf("quicconn: lane: prime: %w", err)
	}
	go closeWith(s.primary, c)
	s.out.addLane(&sendLane{data})
	return nil
}

// closeWith closes lane when primary ends (or lane ends first).
func closeWith(primary, lane *quic.Conn) {
	select {
	case <-primary.Context().Done():
		_ = lane.CloseWithError(0, "")
	case <-lane.Context().Done():
	}
}

// laneTLSConfig is the receiver's TLS config for an extra lane.
func laneTLSConfig() *tls.Config {
	c := ReceiverTLSConfig()
	c.NextProtos = []string{ALPNAttach}
	return c
}

// dialLanes opens the extra lanes of a striped session from the receiver,
// on the transport its first connection runs on. A lane that fails is
// left out: the transfer runs on the others.
func dialLanes(tr *quic.Transport, primary *quic.Conn, binding []byte, in *stripeIn) {
	key, id := laneSecrets(binding)
	for lane := byte(1); int(lane) < laneCount(); lane++ { // laneCount ≤ maxLanes
		in.expectLane()
		go func() {
			if err := dialLane(tr, primary, key, id, lane, in); err != nil {
				in.laneFailed()
			}
		}()
	}
}

func dialLane(tr *quic.Transport, primary *quic.Conn, key []byte, id [laneIDLen]byte, lane byte, in *stripeIn) (err error) {
	ctx, cancel := context.WithTimeout(primary.Context(), HandshakeTimeout)
	defer cancel()
	c, err := tr.Dial(ctx, primary.RemoteAddr(), laneTLSConfig(), QuicConfig())
	if err != nil {
		return err
	}
	defer closeOnFailure(c, &err)
	if c.ConnectionState().TLS.NegotiatedProtocol != ALPNAttach {
		return errors.New("quicconn: lane: peer refused the lane protocol")
	}
	exporter, err := exporterOf(c)
	if err != nil {
		return err
	}
	st, err := c.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	_ = st.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	msg := make([]byte, 0, attachMsgLen)
	msg = append(msg, attachMagic, attachVersion)
	msg = append(msg, id[:]...)
	msg = append(msg, lane)
	msg = append(msg, laneTag(key, dirLaneReceiver, exporter, lane)...)
	if _, err := st.Write(msg); err != nil {
		return err
	}
	var got [tagLen]byte
	if _, err := io.ReadFull(st, got[:]); err != nil {
		return err
	}
	if !hmac.Equal(got[:], laneTag(key, dirLaneSender, exporter, lane)) {
		return fmt.Errorf("%w: lane: wrong sender tag", fserrors.ErrPeerAuthFailed)
	}
	data, err := c.AcceptUniStream(ctx)
	if err != nil {
		return err
	}
	var prime [1]byte
	if _, err := io.ReadFull(data, prime[:]); err != nil {
		return err
	}
	go closeWith(primary, c)
	in.attachLane(&recvLane{data})
	return nil
}

// sendLane and recvLane adapt QUIC streams to lanes.
type sendLane struct{ s *quic.SendStream }

func (l *sendLane) Write(p []byte) (int, error)        { return l.s.Write(p) }
func (l *sendLane) Close() error                       { return l.s.Close() }
func (l *sendLane) SetWriteDeadline(t time.Time) error { return l.s.SetWriteDeadline(t) }
func (l *sendLane) CancelWrite()                       { l.s.CancelWrite(0) }

type recvLane struct{ s *quic.ReceiveStream }

func (l *recvLane) Read(p []byte) (int, error)        { return l.s.Read(p) }
func (l *recvLane) CancelRead()                       { l.s.CancelRead(0) }
func (l *recvLane) SetReadDeadline(t time.Time) error { return l.s.SetReadDeadline(t) }
