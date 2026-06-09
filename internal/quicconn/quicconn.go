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
	"time"

	"github.com/quic-go/quic-go"

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

// pqCurvePreferences pins the key-exchange groups so the post-quantum
// X25519MLKEM768 hybrid is always preferred — set explicitly (not via
// Go's default) so a toolchain default change or GODEBUG=tlsmlkem=0 can't
// silently downgrade us; TestNegotiatesPostQuantum guards the wire result.
// Plain X25519 stays as a classically-secure fallback for peers without
// ML-KEM support.
var pqCurvePreferences = []tls.CurveID{tls.X25519MLKEM768, tls.X25519}

// HandshakeTimeout caps the time we'll spend on the TLS+QUIC handshake.
const HandshakeTimeout = 10 * time.Second

// AcceptResult bundles the negotiated streams with the underlying QUIC
// connection so callers can manage its lifecycle.
type AcceptResult struct {
	Conn    *quic.Conn
	Streams transfer.Streams
}

// Close shuts the streams and the QUIC connection.
func (r *AcceptResult) Close() {
	if r == nil {
		return
	}
	r.Streams.Close()
	if r.Conn != nil {
		_ = r.Conn.CloseWithError(0, "")
	}
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
// step in authenticatePeer, which binds the SPAKE2-derived secret to
// this specific TLS session via the RFC 5705 exporter. A MITM that
// terminates the TLS session ends up with a different exporter and
// fails that tag check.
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
	return &quic.Config{
		HandshakeIdleTimeout:           HandshakeTimeout,
		MaxIdleTimeout:                 30 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		InitialStreamReceiveWindow:     512 * 1024,
		MaxStreamReceiveWindow:         6 * 1024 * 1024,
		InitialConnectionReceiveWindow: 1024 * 1024,
		MaxConnectionReceiveWindow:     15 * 1024 * 1024,
	}
}

// Listener wraps a quic.Listener for the LAN/test path. It carries the
// shared code so each Accept can run the PAKE handshake without an
// extra argument at the call site.
type Listener struct {
	ln   *quic.Listener
	code string
}

// Close stops listening.
func (l *Listener) Close() error { return l.ln.Close() }

// Accept blocks for the next inbound QUIC connection, completes the
// stream handshake on the sender side, and returns the negotiated
// streams.
func (l *Listener) Accept(ctx context.Context) (*AcceptResult, error) {
	c, err := l.ln.Accept(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: accept: %w", err)
	}
	return SenderHandshake(ctx, c, l.code)
}

// SenderHandshake opens the control stream, authenticates the peer via
// SPAKE2 + TLS channel binding, then opens and primes the data stream.
// The prime byte lets the receiver's AcceptUniStream return before the
// first real chunk — without it an empty transfer (e.g. a directory tar
// with no entries) would deadlock the receiver.
func SenderHandshake(ctx context.Context, c *quic.Conn, code string) (*AcceptResult, error) {
	ctrl, err := c.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: open control: %w", err)
	}
	// Bound the handshake reads so a peer that connects and then stalls
	// can't pin this side until the 30s QUIC idle timeout. Cleared on
	// success so the transfer's own control reads are not capped.
	_ = ctrl.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	if err := authenticatePeer(c, ctrl, code, roleSender); err != nil {
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
		},
	}, nil
}

// ListenAddr binds a QUIC listener on addr. code is the shared short
// code used to authenticate every incoming peer.
func ListenAddr(addr, code string) (*Listener, error) {
	tlsCfg, err := SenderTLSConfig()
	if err != nil {
		return nil, err
	}
	ln, err := quic.ListenAddr(addr, tlsCfg, QuicConfig())
	if err != nil {
		return nil, fmt.Errorf("quicconn: listen: %w", err)
	}
	return &Listener{ln: ln, code: code}, nil
}

// Dial connects to a sender-side QUIC listener and returns the
// negotiated streams from the receiver's perspective. code is the
// shared short code used to authenticate the sender.
func Dial(ctx context.Context, addr, code string) (*AcceptResult, error) {
	hsCtx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()

	c, err := quic.DialAddr(hsCtx, addr, ReceiverTLSConfig(), QuicConfig())
	if err != nil {
		return nil, fmt.Errorf("quicconn: dial: %w", err)
	}
	return ReceiverHandshake(ctx, c, code)
}

// ReceiverHandshake accepts the sender's control stream, authenticates
// the peer via SPAKE2 + TLS channel binding, then accepts the data
// stream and consumes the priming byte.
func ReceiverHandshake(ctx context.Context, c *quic.Conn, code string) (*AcceptResult, error) {
	ctrl, err := c.AcceptStream(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: accept control: %w", err)
	}
	// Bound the handshake reads (see SenderHandshake); cleared on success.
	_ = ctrl.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	if err := authenticatePeer(c, ctrl, code, roleReceiver); err != nil {
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
		},
	}, nil
}

// streamCloser wraps a bidi quic.Stream so the transfer package can
// treat it as an io.ReadWriteCloser. Close() shuts the write side and
// lets reads continue until peer EOF, matching the wire-protocol
// shutdown contract.
type streamCloser struct{ stream *quic.Stream }

func (s *streamCloser) Read(p []byte) (int, error)  { return s.stream.Read(p) }
func (s *streamCloser) Write(p []byte) (int, error) { return s.stream.Write(p) }
func (s *streamCloser) Close() error                { return s.stream.Close() }

// uniInCloser adapts a quic.ReceiveStream (read-only) to io.ReadWriteCloser.
type uniInCloser struct{ s *quic.ReceiveStream }

func (u *uniInCloser) Read(p []byte) (int, error) { return u.s.Read(p) }
func (u *uniInCloser) Write(_ []byte) (int, error) {
	return 0, errors.New("quicconn: stream is receive-only")
}
func (u *uniInCloser) Close() error { u.s.CancelRead(0); return nil }

func wrapUniIn(s *quic.ReceiveStream) io.ReadWriteCloser { return &uniInCloser{s: s} }

// uniOutCloser adapts a quic.SendStream (write-only) to io.ReadWriteCloser.
type uniOutCloser struct{ s *quic.SendStream }

func (u *uniOutCloser) Read(_ []byte) (int, error)  { return 0, io.EOF }
func (u *uniOutCloser) Write(p []byte) (int, error) { return u.s.Write(p) }
func (u *uniOutCloser) Close() error                { return u.s.Close() }

func wrapUniOut(s *quic.SendStream) io.ReadWriteCloser { return &uniOutCloser{s: s} }

// selfSignedCert returns a fresh self-signed Ed25519 cert valid for 1 hour.
func selfSignedCert() (tls.Certificate, error) {
	pub, priv, err := generateEd25519()
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
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
