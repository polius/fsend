// Package quicconn wraps quic-go to expose the three logical streams
// (control, data, receiver-control) that the transfer engine consumes.
//
// Critical wiring note (from PROJECT_SPEC.md "Critical implementation
// note"): when the underlying socket comes from an ICE-established UDP
// connection, we hand it to quic-go via Transport.Conn — we do NOT let
// quic-go open its own socket, or the punched NAT mapping is wasted.
//
// This package supports both modes:
//
//   - Listen(packetConn) / Dial(packetConn, ...) for the production path
//     where ICE provides the socket.
//   - ListenAddr / DialAddr for tests and the LAN MVP path where we just
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
	"net"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/transfer"
)

// ALPN is the application-layer protocol name we negotiate over TLS-in-QUIC.
// Bumping this is a wire-protocol-major event.
const ALPN = "fsend/1"

// HandshakeTimeout caps the time we'll spend on the TLS+QUIC handshake.
const HandshakeTimeout = 10 * time.Second

// streamPair packages quic-go's bidirectional/unidirectional stream into
// the io.ReadWriteCloser shape the transfer package expects.
type streamCloser struct {
	stream *quic.Stream
}

func (s *streamCloser) Read(p []byte) (int, error)  { return s.stream.Read(p) }
func (s *streamCloser) Write(p []byte) (int, error) { return s.stream.Write(p) }
func (s *streamCloser) Close() error {
	// Close the write side; let read continue until peer closes too.
	if err := s.stream.Close(); err != nil {
		return err
	}
	return nil
}

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

// senderTLSConfig builds a TLS config for the sender (QUIC server-role in
// our protocol — see docs/decisions/wire-protocol.md). The sender always
// opens the listening side; the receiver dials in.
//
// PSK auth via the PAKE-derived key is the real mutual authentication
// mechanism (see docs/security/threat-model.md). We use a self-signed
// per-session cert so QUIC can complete its TLS handshake; certificate
// verification is effectively no-op because we trust the PAKE key.
func senderTLSConfig() (*tls.Config, error) {
	cert, err := selfSignedCert()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates:       []tls.Certificate{cert},
		NextProtos:         []string{ALPN},
		MinVersion:         tls.VersionTLS13,
		ClientAuth:         tls.NoClientCert,
	}, nil
}

// receiverTLSConfig is the client-role TLS config — the receiver dials.
//
// InsecureSkipVerify is set because the sender's cert is self-signed; the
// actual MITM defense is the PAKE channel binding (see docs/security/
// threat-model.md). When the PAKE key disagrees, the TLS exporter values
// disagree, and the application-layer password-style check that runs
// inside the encrypted tunnel fails. In v0.1.0 we depend on that check
// happening at the transfer-protocol layer (HELLO/PASSWORD frames); the
// proper RFC 5705 exporter-based binding is in the wire spec for the
// next iteration.
func receiverTLSConfig() *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // PAKE channel binding is the real auth
		NextProtos:         []string{ALPN},
		MinVersion:         tls.VersionTLS13,
	}
}

// quicConfig returns the shared quic-go config (timeouts and limits per
// docs/decisions/implementation-defaults.md).
func quicConfig() *quic.Config {
	return &quic.Config{
		HandshakeIdleTimeout:           HandshakeTimeout,
		MaxIdleTimeout:                 30 * time.Second,
		KeepAlivePeriod:                10 * time.Second,
		InitialStreamReceiveWindow:     512 * 1024,
		MaxStreamReceiveWindow:         6 * 1024 * 1024,
		InitialConnectionReceiveWindow: 1024 * 1024,
		MaxConnectionReceiveWindow:     15 * 1024 * 1024,
		EnableDatagrams:                false,
		Allow0RTT:                      false,
	}
}

// ListenAddr opens a QUIC listener on the given UDP address (e.g. ":0")
// and returns a Listener plus the actual address it bound to.
//
// Used by the sender in the LAN MVP path: the sender announces this
// address via mDNS, then receiver dials it.
type Listener struct {
	ln *quic.Listener
}

// LocalAddr returns the UDP address the listener is bound to.
func (l *Listener) LocalAddr() net.Addr { return l.ln.Addr() }

// Close stops listening.
func (l *Listener) Close() error { return l.ln.Close() }

// Accept blocks until the next inbound QUIC connection completes its
// handshake, then sets up the three logical streams on the sender side.
//
// Stream ownership (per docs/decisions/wire-protocol.md):
//   - Control:         bidirectional, opened by SENDER
//   - Data:            unidirectional sender→receiver, opened by SENDER
//   - ReceiverControl: unidirectional receiver→sender, opened by RECEIVER
//
// QUIC streams only become visible to the peer once written, so we open
// our outbound streams AND accept the peer's inbound stream in parallel
// to avoid a circular wait. A single zero-byte priming write on each
// outbound stream ensures it actually appears on the wire.
func (l *Listener) Accept(ctx context.Context) (*AcceptResult, error) {
	c, err := l.ln.Accept(ctx)
	if err != nil {
		return nil, fmt.Errorf("quicconn: accept: %w", err)
	}
	return senderHandshake(ctx, c)
}

func senderHandshake(ctx context.Context, c *quic.Conn) (*AcceptResult, error) {
	type result struct {
		ctrl     *quic.Stream
		dataOut  *quic.SendStream
		rcvIn    *quic.ReceiveStream
		err      error
	}
	done := make(chan result, 3)

	go func() {
		r := result{}
		s, err := c.OpenStreamSync(ctx)
		if err != nil {
			r.err = fmt.Errorf("open control: %w", err)
			done <- r
			return
		}
		// Prime so the receiver's Accept returns.
		if _, err := s.Write([]byte{0}); err != nil {
			r.err = fmt.Errorf("prime control: %w", err)
			done <- r
			return
		}
		r.ctrl = s
		done <- r
	}()
	go func() {
		r := result{}
		s, err := c.OpenUniStreamSync(ctx)
		if err != nil {
			r.err = fmt.Errorf("open data: %w", err)
			done <- r
			return
		}
		if _, err := s.Write([]byte{0}); err != nil {
			r.err = fmt.Errorf("prime data: %w", err)
			done <- r
			return
		}
		r.dataOut = s
		done <- r
	}()
	go func() {
		r := result{}
		s, err := c.AcceptUniStream(ctx)
		if err != nil {
			r.err = fmt.Errorf("accept receiver-control: %w", err)
			done <- r
			return
		}
		// Consume the receiver's priming byte.
		var b [1]byte
		if _, err := s.Read(b[:]); err != nil {
			r.err = fmt.Errorf("read receiver-control prime: %w", err)
			done <- r
			return
		}
		r.rcvIn = s
		done <- r
	}()

	var out AcceptResult
	out.Conn = c
	for i := 0; i < 3; i++ {
		r := <-done
		if r.err != nil {
			return nil, fmt.Errorf("quicconn: sender handshake: %w", r.err)
		}
		switch {
		case r.ctrl != nil:
			// Consume receiver's priming byte on the bidi stream.
			var b [1]byte
			if _, err := r.ctrl.Read(b[:]); err != nil {
				return nil, fmt.Errorf("quicconn: read control prime: %w", err)
			}
			out.Streams.Control = &streamCloser{stream: r.ctrl}
		case r.dataOut != nil:
			out.Streams.Data = wrapUniOut(r.dataOut)
		case r.rcvIn != nil:
			out.Streams.ReceiverControl = wrapUniIn(r.rcvIn)
		}
	}
	return &out, nil
}

// ListenAddr binds a QUIC listener on addr.
func ListenAddr(addr string) (*Listener, error) {
	tlsCfg, err := senderTLSConfig()
	if err != nil {
		return nil, err
	}
	ln, err := quic.ListenAddr(addr, tlsCfg, quicConfig())
	if err != nil {
		return nil, fmt.Errorf("quicconn: listen: %w", err)
	}
	return &Listener{ln: ln}, nil
}

// Dial connects to a sender-side QUIC listener and returns the negotiated
// streams from the receiver's perspective.
//
// Same parallel-open/accept pattern as Listener.Accept (see comment there).
func Dial(ctx context.Context, addr string) (*AcceptResult, error) {
	hsCtx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()

	c, err := quic.DialAddr(hsCtx, addr, receiverTLSConfig(), quicConfig())
	if err != nil {
		return nil, fmt.Errorf("quicconn: dial: %w", err)
	}
	return receiverHandshake(ctx, c)
}

func receiverHandshake(ctx context.Context, c *quic.Conn) (*AcceptResult, error) {
	type result struct {
		ctrl     *quic.Stream
		dataIn   *quic.ReceiveStream
		rcvOut   *quic.SendStream
		err      error
	}
	done := make(chan result, 3)

	go func() {
		r := result{}
		s, err := c.AcceptStream(ctx)
		if err != nil {
			r.err = fmt.Errorf("accept control: %w", err)
			done <- r
			return
		}
		// Consume sender's priming byte.
		var b [1]byte
		if _, err := s.Read(b[:]); err != nil {
			r.err = fmt.Errorf("read control prime: %w", err)
			done <- r
			return
		}
		// Send our priming byte back so the sender's read completes.
		if _, err := s.Write([]byte{0}); err != nil {
			r.err = fmt.Errorf("prime control: %w", err)
			done <- r
			return
		}
		r.ctrl = s
		done <- r
	}()
	go func() {
		r := result{}
		s, err := c.AcceptUniStream(ctx)
		if err != nil {
			r.err = fmt.Errorf("accept data: %w", err)
			done <- r
			return
		}
		var b [1]byte
		if _, err := s.Read(b[:]); err != nil {
			r.err = fmt.Errorf("read data prime: %w", err)
			done <- r
			return
		}
		r.dataIn = s
		done <- r
	}()
	go func() {
		r := result{}
		s, err := c.OpenUniStreamSync(ctx)
		if err != nil {
			r.err = fmt.Errorf("open receiver-control: %w", err)
			done <- r
			return
		}
		if _, err := s.Write([]byte{0}); err != nil {
			r.err = fmt.Errorf("prime receiver-control: %w", err)
			done <- r
			return
		}
		r.rcvOut = s
		done <- r
	}()

	var out AcceptResult
	out.Conn = c
	for i := 0; i < 3; i++ {
		r := <-done
		if r.err != nil {
			return nil, fmt.Errorf("quicconn: receiver handshake: %w", r.err)
		}
		switch {
		case r.ctrl != nil:
			out.Streams.Control = &streamCloser{stream: r.ctrl}
		case r.dataIn != nil:
			out.Streams.Data = wrapUniIn(r.dataIn)
		case r.rcvOut != nil:
			out.Streams.ReceiverControl = wrapUniOut(r.rcvOut)
		}
	}
	return &out, nil
}

// uniInCloser adapts a quic.ReceiveStream (read-only) to io.ReadWriteCloser
// where Write is a no-op error.
type uniInCloser struct{ s *quic.ReceiveStream }

func (u *uniInCloser) Read(p []byte) (int, error)  { return u.s.Read(p) }
func (u *uniInCloser) Write(_ []byte) (int, error) { return 0, errors.New("quicconn: stream is receive-only") }
func (u *uniInCloser) Close() error                { u.s.CancelRead(0); return nil }

func wrapUniIn(s *quic.ReceiveStream) io.ReadWriteCloser { return &uniInCloser{s: s} }

// uniOutCloser adapts a quic.SendStream (write-only) to io.ReadWriteCloser
// where Read returns EOF immediately.
type uniOutCloser struct{ s *quic.SendStream }

func (u *uniOutCloser) Read(_ []byte) (int, error)  { return 0, io.EOF }
func (u *uniOutCloser) Write(p []byte) (int, error) { return u.s.Write(p) }
func (u *uniOutCloser) Close() error                { return u.s.Close() }

func wrapUniOut(s *quic.SendStream) io.ReadWriteCloser { return &uniOutCloser{s: s} }

// selfSignedCert returns a fresh self-signed Ed25519 cert valid for 1 hour.
//
// The session cert is throwaway; PAKE channel binding is the real auth.
func selfSignedCert() (tls.Certificate, error) {
	// Use ECDSA for compatibility with TLS 1.3.
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
