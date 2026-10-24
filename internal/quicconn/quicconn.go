// Package quicconn wraps quic-go to expose the two logical streams
// (control + data) that the transfer engine consumes.
//
// Critical wiring note: when the underlying socket comes from an
// ICE-established UDP connection, we hand it to quic-go via
// Transport.Conn — we do NOT let quic-go open its own socket, or the
// punched NAT mapping is wasted.
//
// This package supports both modes:
//
//   - SenderHandshake / ReceiverHandshake for the production path
//     where ICE or the relay provides the socket and the caller drives
//     a quic.Transport directly.
//   - ListenAddr / Dial for tests and the LAN MVP path where we just
//     want quic-go to manage its own UDP socket.
package quicconn

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/transfer"
)

// quic-go logs a one-line warning to the default logger every time the
// PacketConn it gets isn't a *net.UDPConn — which is exactly the case on
// our internet path, where the conn handed in is either the relay
// wrapper or an ICE-owning wrapper. The warning is informational (it
// just means quic-go can't tune SO_RCVBUF/SO_SNDBUF through us), and on
// the relay path the underlying socket couldn't honor those settings
// usefully anyway. Silence it with the library's own env-var knob so
// nothing leaks onto user stderr — most visibly under --quiet, where the
// e2e suite asserts on an empty stream.
//
// We only set the variable when the user hasn't already set it; an
// explicit deployment override (e.g. "0") wins.
func init() {
	if os.Getenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING") == "" {
		_ = os.Setenv("QUIC_GO_DISABLE_RECEIVE_BUFFER_WARNING", "1")
	}
}

// ALPN is the application-layer protocol name we negotiate over TLS-in-QUIC.
// Bumping this is a wire-protocol-major event.
const ALPN = "fsend/1"

// pqCurvePreferences enforces the post-quantum X25519MLKEM768 hybrid key
// exchange. Both peers are always fsend v3 builds, so there is no classical
// fallback to offer and none an attacker could downgrade to;
// TestNegotiatesPostQuantum guards the wire result.
var pqCurvePreferences = []tls.CurveID{tls.X25519MLKEM768}

// HandshakeTimeout caps the time we'll spend on the TLS+QUIC handshake,
// and separately on the PAKE handshake that follows it.
const HandshakeTimeout = 10 * time.Second

// Application close codes. 0 is a plain close.
const (
	// CloseCancelled tells the peer this side was deliberately cancelled
	// (Ctrl-C), so it can report E032 at once instead of retrying.
	CloseCancelled quic.ApplicationErrorCode = 0x10
	// closeHandshakeFailed ends a connection whose PAKE handshake failed,
	// so neither side keeps a dead connection alive on keepalives.
	closeHandshakeFailed quic.ApplicationErrorCode = 0x11
	// closeBusy rejects a connection while too many handshakes are pending.
	closeBusy quic.ApplicationErrorCode = 0x12
)

// AcceptResult bundles the negotiated streams with the underlying QUIC
// connection so callers can manage its lifecycle.
type AcceptResult struct {
	Conn    *quic.Conn
	Streams transfer.Streams
}

// Close shuts the streams and the QUIC connection.
func (r *AcceptResult) Close() { r.closeWith(0, "") }

// Cancel closes the connection with CloseCancelled so the peer learns this
// side quit on purpose. CloseWithError returns once the close is sent.
func (r *AcceptResult) Cancel() { r.closeWith(CloseCancelled, "cancelled") }

func (r *AcceptResult) closeWith(code quic.ApplicationErrorCode, msg string) {
	if r == nil {
		return
	}
	r.Streams.Close()
	if r.Conn != nil {
		_ = r.Conn.CloseWithError(code, msg)
	}
}

// PeerCancelled reports whether the peer closed the connection with
// CloseCancelled, waiting up to grace for its close to arrive: a stream
// error usually surfaces a moment before the connection-level close.
func (r *AcceptResult) PeerCancelled(grace time.Duration) bool {
	if r == nil || r.Conn == nil {
		return false
	}
	t := time.NewTimer(grace)
	defer t.Stop()
	select {
	case <-r.Conn.Context().Done():
	case <-t.C:
		return false
	}
	return IsCancelClose(context.Cause(r.Conn.Context()))
}

// IsCancelClose reports whether err is the peer's CloseCancelled close.
func IsCancelClose(err error) bool {
	var appErr *quic.ApplicationError
	return errors.As(err, &appErr) && appErr.Remote && appErr.ErrorCode == CloseCancelled
}

// SenderTLSConfig builds a TLS config for the sender (QUIC server-role).
// The sender always opens the listening side; the receiver dials in.
func SenderTLSConfig() (*tls.Config, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:     []tls.Certificate{cert},
		NextProtos:       []string{ALPN},
		MinVersion:       tls.VersionTLS13,
		CurvePreferences: pqCurvePreferences,
		ClientAuth:       tls.NoClientCert,
	}, nil
}

// ReceiverTLSConfig is the client-role TLS config — the receiver dials.
//
// InsecureSkipVerify is set because the sender's cert is self-signed:
// peer identity is established afterwards by the PAKE channel-binding
// step in authenticatePeer, which binds the CPace-derived secret to
// this specific TLS session via the RFC 5705 exporter. A MITM that
// terminates the TLS session ends up with a different exporter and
// fails that tag check — including the pairing server itself, which
// never learns the code (it sees only the argon2id slot) and so cannot
// run the PAKE with either side.
func ReceiverTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true,
		NextProtos:         []string{ALPN},
		MinVersion:         tls.VersionTLS13,
		CurvePreferences:   pqCurvePreferences,
	}
}

// QuicConfig returns the shared quic-go config.
func QuicConfig() *quic.Config {
	idle := 30 * time.Second
	// Undocumented test knob: peer-death detection latency equals the
	// idle timeout, and the e2e suite can't wait 30 s per scenario.
	if v := os.Getenv("FSEND_QUIC_IDLE_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			idle = d
		}
	}
	keepAlive := 10 * time.Second
	if keepAlive > idle/2 {
		keepAlive = idle / 2
	}
	// Windows start small and quic-go auto-tunes them up to the max, so a
	// large max only costs memory on paths whose BDP needs it. 64 MiB keeps
	// the single data stream near 1.7 Gbps at 300 ms RTT.
	return &quic.Config{
		HandshakeIdleTimeout:           HandshakeTimeout,
		MaxIdleTimeout:                 idle,
		KeepAlivePeriod:                keepAlive,
		InitialStreamReceiveWindow:     512 * 1024,
		MaxStreamReceiveWindow:         64 * 1024 * 1024,
		InitialConnectionReceiveWindow: 1024 * 1024,
		MaxConnectionReceiveWindow:     80 * 1024 * 1024,
	}
}

// maxPendingHandshakes caps concurrent PAKE handshakes per listener, so a
// connection flood can't pile up goroutines and live connections.
const maxPendingHandshakes = 8

// Listener wraps a quic.Listener and runs the PAKE handshake for each
// inbound connection in its own goroutine, bounded by HandshakeTimeout:
// a stalling impostor can't block the real receiver queued behind it.
type Listener struct {
	ln *quic.Listener

	mu     sync.Mutex
	secret string

	results chan *AcceptResult
	fails   chan error // failed handshakes; dropped when full
	done    chan struct{}
	err     error // set before done closes
	once    sync.Once
}

// NewListener wraps ln, authenticating each peer with secret.
func NewListener(ln *quic.Listener, secret string) *Listener {
	l := &Listener{
		ln:      ln,
		secret:  secret,
		results: make(chan *AcceptResult),
		fails:   make(chan error, 64),
		done:    make(chan struct{}),
	}
	go l.acceptLoop()
	return l
}

// SetSecret changes the PAKE secret for handshakes that start afterwards.
func (l *Listener) SetSecret(secret string) {
	l.mu.Lock()
	l.secret = secret
	l.mu.Unlock()
}

func (l *Listener) currentSecret() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.secret
}

// Close stops listening and abandons pending handshakes.
func (l *Listener) Close() error {
	err := l.ln.Close()
	l.stop(errors.New("quicconn: listener closed"))
	return err
}

func (l *Listener) stop(err error) {
	l.once.Do(func() {
		l.err = err
		close(l.done)
	})
}

func (l *Listener) acceptLoop() {
	sem := make(chan struct{}, maxPendingHandshakes)
	for {
		c, err := l.ln.Accept(context.Background())
		if err != nil {
			l.stop(fmt.Errorf("quicconn: accept: %w", err))
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			_ = c.CloseWithError(closeBusy, "busy")
			continue
		}
		go func() {
			defer func() { <-sem }()
			res, err := SenderHandshake(context.Background(), c, l.currentSecret())
			if err != nil {
				select {
				case l.fails <- fmt.Errorf("%w: %w", ErrHandshakeFailed, err):
				default:
				}
				return
			}
			select {
			case l.results <- res:
			case <-l.done:
				res.Close()
			}
		}()
	}
}

// ErrHandshakeFailed wraps one peer's failed handshake as returned by
// Listener.Accept: the listener itself is still accepting.
var ErrHandshakeFailed = errors.New("quicconn: peer handshake failed")

// Accept returns the next authenticated connection. A failed handshake
// (an impostor, a wrong code) comes back as an ErrHandshakeFailed error —
// also wrapping fserrors.ErrPeerAuthFailed when the PAKE itself failed —
// so the caller can count it and keep accepting.
func (l *Listener) Accept(ctx context.Context) (*AcceptResult, error) {
	select {
	case res := <-l.results:
		return res, nil
	case err := <-l.fails:
		return nil, err
	case <-l.done:
		return nil, l.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// SenderHandshake opens the control stream, authenticates the peer via
// CPace + TLS channel binding, then opens and primes the data stream.
// The prime byte lets the receiver's AcceptUniStream return before the
// first real chunk — without it an empty transfer (e.g. a directory tar
// with no entries) would deadlock the receiver.
//
// The handshake is bounded by HandshakeTimeout, and a failure closes the
// connection: keepalives would otherwise hold it open.
func SenderHandshake(ctx context.Context, c *quic.Conn, secret string) (res *AcceptResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	defer closeOnFailure(c, &err)
	ctrl, err := c.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: open control: %w", err)
	}
	// Bound the handshake reads so a peer that connects and then stalls
	// can't pin this side until the 30s QUIC idle timeout. Cleared on
	// success so the transfer's own control reads are not capped.
	_ = ctrl.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	binding, err := authenticatePeer(c, ctrl, secret, roleSender)
	if err != nil {
		return nil, err
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	data, err := c.OpenUniStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: open data: %w", err)
	}
	if _, err := data.Write([]byte{0}); err != nil {
		return nil, fmt.Errorf("quicconn: prime data: %w", err)
	}
	return &AcceptResult{
		Conn: c,
		Streams: transfer.Streams{
			Control: &streamCloser{stream: ctrl},
			Data:    wrapUniOut(data),
			Binding: binding,
		},
	}, nil
}

// closeOnFailure closes c when *errp is set, so a failed handshake never
// leaves a live connection behind. A peer that closed with
// closeHandshakeFailed rejected us — its close can overtake its last
// PAKE message — so that surfaces as ErrPeerAuthFailed.
func closeOnFailure(c *quic.Conn, errp *error) {
	if *errp == nil {
		return
	}
	var appErr *quic.ApplicationError
	if errors.As(*errp, &appErr) && appErr.Remote && appErr.ErrorCode == closeHandshakeFailed &&
		!errors.Is(*errp, fserrors.ErrPeerAuthFailed) {
		*errp = fmt.Errorf("%w: %v", fserrors.ErrPeerAuthFailed, *errp)
	}
	_ = c.CloseWithError(closeHandshakeFailed, "handshake failed")
}

// ListenAddr binds a QUIC listener on addr. secret is the PAKE secret
// (the short code, or a resume secret) every incoming peer must know.
func ListenAddr(addr, secret string) (*Listener, error) {
	tlsCfg, err := SenderTLSConfig()
	if err != nil {
		return nil, err
	}
	ln, err := quic.ListenAddr(addr, tlsCfg, QuicConfig())
	if err != nil {
		return nil, fmt.Errorf("quicconn: listen: %w", err)
	}
	return NewListener(ln, secret), nil
}

// Dial connects to a sender-side QUIC listener and returns the
// negotiated streams from the receiver's perspective. secret is the
// PAKE secret used to authenticate the sender.
func Dial(ctx context.Context, addr, secret string) (*AcceptResult, error) {
	return DialTimeout(ctx, addr, secret, HandshakeTimeout)
}

// DialTimeout is Dial with a separate budget for the QUIC connect. Once
// QUIC is up the sender is reachable, so the PAKE always gets the full
// HandshakeTimeout: abandoning it midway could strand a sender that has
// already committed to this connection.
func DialTimeout(ctx context.Context, addr, secret string, connectTimeout time.Duration) (*AcceptResult, error) {
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	c, err := quic.DialAddr(cctx, addr, ReceiverTLSConfig(), QuicConfig())
	cancel()
	if err != nil {
		return nil, fmt.Errorf("quicconn: dial: %w", err)
	}
	return ReceiverHandshake(ctx, c, secret)
}

// ReceiverHandshake accepts the sender's control stream, authenticates
// the peer via CPace + TLS channel binding, then accepts the data
// stream and consumes the priming byte. Bounded and closed on failure
// like SenderHandshake.
func ReceiverHandshake(ctx context.Context, c *quic.Conn, secret string) (res *AcceptResult, err error) {
	ctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()
	defer closeOnFailure(c, &err)
	ctrl, err := c.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: accept control: %w", err)
	}
	// Bound the handshake reads (see SenderHandshake); cleared on success.
	_ = ctrl.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	binding, err := authenticatePeer(c, ctrl, secret, roleReceiver)
	if err != nil {
		return nil, err
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	data, err := c.AcceptUniStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: accept data: %w", err)
	}
	var prime [1]byte
	if _, err := io.ReadFull(data, prime[:]); err != nil {
		return nil, fmt.Errorf("quicconn: read data prime: %w", err)
	}
	return &AcceptResult{
		Conn: c,
		Streams: transfer.Streams{
			Control: &streamCloser{stream: ctrl},
			Data:    wrapUniIn(data),
			Binding: binding,
		},
	}, nil
}

// streamCloser wraps a bidi quic.Stream so the transfer package can
// treat it as an io.ReadWriteCloser. Close() shuts the write side and
// lets reads continue until peer EOF, matching the wire-protocol
// shutdown contract.
type streamCloser struct{ stream *quic.Stream }

func (s *streamCloser) Read(p []byte) (int, error)         { return s.stream.Read(p) }
func (s *streamCloser) Write(p []byte) (int, error)        { return s.stream.Write(p) }
func (s *streamCloser) Close() error                       { return s.stream.Close() }
func (s *streamCloser) SetReadDeadline(t time.Time) error  { return s.stream.SetReadDeadline(t) }
func (s *streamCloser) SetWriteDeadline(t time.Time) error { return s.stream.SetWriteDeadline(t) }

// uniInCloser adapts a quic.ReceiveStream (read-only) to io.ReadWriteCloser.
type uniInCloser struct{ s *quic.ReceiveStream }

func (u *uniInCloser) Read(p []byte) (int, error) { return u.s.Read(p) }
func (u *uniInCloser) Write(_ []byte) (int, error) {
	return 0, errors.New("quicconn: stream is receive-only")
}
func (u *uniInCloser) Close() error                      { u.s.CancelRead(0); return nil }
func (u *uniInCloser) SetReadDeadline(t time.Time) error { return u.s.SetReadDeadline(t) }

func wrapUniIn(s *quic.ReceiveStream) io.ReadWriteCloser { return &uniInCloser{s: s} }

// uniOutCloser adapts a quic.SendStream (write-only) to io.ReadWriteCloser.
type uniOutCloser struct{ s *quic.SendStream }

func (u *uniOutCloser) Read(_ []byte) (int, error)         { return 0, io.EOF }
func (u *uniOutCloser) Write(p []byte) (int, error)        { return u.s.Write(p) }
func (u *uniOutCloser) Close() error                       { return u.s.Close() }
func (u *uniOutCloser) SetWriteDeadline(t time.Time) error { return u.s.SetWriteDeadline(t) }

func wrapUniOut(s *quic.SendStream) io.ReadWriteCloser { return &uniOutCloser{s: s} }

// selfSignedCert returns a fresh self-signed Ed25519 cert valid for 1 hour.
func selfSignedCert() (tls.Certificate, error) {
	pub, priv, err := generateEd25519()
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicconn: cert serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-1 * time.Minute),
		NotAfter:     time.Now().Add(1 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("quicconn: create cert: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  priv,
		Leaf: &x509.Certificate{
			NotBefore: template.NotBefore,
			NotAfter:  template.NotAfter,
		},
	}, nil
}
