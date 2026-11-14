package quicconn

// The local-network TCP route (audit E1). QUIC in user space sends one
// small datagram per system call, which held a same-network transfer to
// about 72 MB/s with ~25 s of kernel time per GiB on each side; kernel TCP
// gets segmentation offload everywhere. The sender listens for TCP on the
// same code-derived port as its QUIC listener, and a receiver tries TCP
// first, falling back to QUIC (see cmd/fsend). Older peers never use TCP,
// so mixed versions keep working.
//
// Security is the QUIC route's: the same TLS 1.3 settings (post-quantum
// group, ephemeral unverified certificates), and the same CPace exchange
// bound to the TLS exporter of the control connection (authenticatePeer).
//
// Two TLS connections carry a transfer, so the bulk data keeps the kernel
// path to itself:
//   - control: framed (frameData, frameFIN, frameClose, framePing). The
//     frames give it what a QUIC stream and connection have: an end of
//     stream separate from the connection's close, a close code (a cancel
//     must reach the peer as one), and keepalives with an idle timeout.
//   - data: raw bytes, sender to receiver. The receiver opens it after the
//     PAKE and proves it belongs to the session with a tag over the
//     session binding and this connection's own exporter; the sender
//     answers with its own tag. A MITM terminating either TLS connection
//     sees different exporters on each side and can't produce the tags.

import (
	"bufio"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/transfer"
)

// TCPALPN is the TLS application protocol of the TCP route. It differs
// from the QUIC one, so neither route can be confused for the other, and
// an unrelated TLS service on the port fails the handshake.
const TCPALPN = "fsend-tcp/1"

// The first byte a receiver sends on a new TCP route connection.
const (
	tcpRoleControl byte = 'C'
	tcpRoleData    byte = 'D'
)

// Control frame kinds: [kind u8][length u32][payload].
const (
	frameData  byte = 0 // stream bytes
	frameFIN   byte = 1 // end of the stream (Close), the connection stays up
	frameClose byte = 2 // the connection closes: code u64, then a message
	framePing  byte = 3 // keepalive
)

// maxFramePayload bounds one control frame; Write splits larger writes.
const maxFramePayload = 64 << 10

// ctrlBufMax bounds control bytes buffered ahead of the reader. Control
// traffic is small and read promptly (a full buffer only pauses the
// peer); the bound keeps one connection, even before the PAKE, from
// holding much memory.
const ctrlBufMax = 1 << 20

// tagLabelData labels the data connection's tags.
const tagLabelData = "fsend-tcp-data-v1"

// tcpTLSTimeout bounds the TLS handshake on the TCP route. It takes
// milliseconds on a local network; the bound is for something else
// accepting the connect on that port and then saying nothing, which
// should cost the receiver little before QUIC.
const tcpTLSTimeout = 2 * time.Second

// maxPendingData caps TCP data connections being handshaken at once.
const maxPendingData = 8

// maxPreTLS caps TCP route connections still in their TLS handshake or
// hello, so a flood of idle connects can't exhaust file descriptors (each
// holds one, unlike a QUIC handshake).
const maxPreTLS = 32

// pendingWait is how long a data connection waits for its session to be
// registered: the receiver can finish the PAKE (and dial) a moment before
// the sender has read the receiver's tag.
const pendingWait = 2 * time.Second

func tcpTLS(c *tls.Config) *tls.Config {
	c = c.Clone()
	c.NextProtos = []string{TCPALPN}
	return c
}

// ErrTCPData marks a failure of the data connection, after the PAKE on the
// control connection succeeded. It is never a wrong code: the receiver
// falls back to QUIC, and the sender doesn't count it against the code
// (a MITM on the data connection, or its slots kept full, costs only the
// TCP route).
var ErrTCPData = errors.New("quicconn: tcp data connection failed")

// errTCPProtocol is a malformed frame or hello on the TCP route.
var errTCPProtocol = errors.New("quicconn: tcp route protocol error")

// tcpConn is a transfer's pair of TCP route connections. It implements
// Conn: its context ends when the control connection does, with a peer's
// close code as the cause, as a QUIC connection's would.
type tcpConn struct {
	ctrl *tls.Conn

	dmu  sync.Mutex
	data *tls.Conn // nil until the data connection is attached

	ctx    context.Context
	cancel context.CancelCauseFunc

	in    ctrlIn
	out   chan outFrame // control frames to send, in order
	wdone chan struct{} // the writer stopped

	closeOnce sync.Once
}

type outFrame struct {
	b    []byte
	done chan error // nil: nobody waits (pings)
}

func newTCPConn(ctrl *tls.Conn) *tcpConn {
	ctx, cancel := context.WithCancelCause(context.Background())
	c := &tcpConn{
		ctrl:   ctrl,
		ctx:    ctx,
		cancel: cancel,
		out:    make(chan outFrame, 16),
		wdone:  make(chan struct{}),
	}
	c.in.init()
	go c.writeLoop()
	go c.readLoop()
	go c.pingLoop()
	return c
}

func (c *tcpConn) Context() context.Context { return c.ctx }

// setData attaches the data connection; if the connection already ended,
// it is closed at once.
func (c *tcpConn) setData(dc *tls.Conn) {
	c.dmu.Lock()
	c.data = dc
	c.dmu.Unlock()
	if c.ctx.Err() != nil {
		_ = dc.Close()
	}
}

// CloseWithError sends the close frame (best effort, bounded), then shuts
// both connections. Like QUIC's, it returns once the close is sent.
func (c *tcpConn) CloseWithError(code quic.ApplicationErrorCode, msg string) error {
	c.closeOnce.Do(func() {
		if c.ctx.Err() == nil {
			p := make([]byte, 8+len(msg))
			binary.BigEndian.PutUint64(p, uint64(code))
			copy(p[8:], msg)
			done := make(chan error, 1)
			select {
			case c.out <- outFrame{b: frame(frameClose, p), done: done}:
				select {
				case err := <-done:
					_ = err // best effort: the connection closes either way
				case <-time.After(time.Second):
				}
			case <-time.After(time.Second):
			}
		}
		cause := &quic.ApplicationError{ErrorCode: code, ErrorMessage: msg}
		c.in.setErr(cause) // a pending control Read returns, as on QUIC
		c.cancel(cause)
		c.shutdown()
	})
	return nil
}

// shutdown closes both connections. The control connection's write side
// closes first, so the peer reads everything sent before EOF; the read
// loop is still draining what the peer sent, so the close is not a reset.
func (c *tcpConn) shutdown() {
	c.dmu.Lock()
	dc := c.data
	c.dmu.Unlock()
	if dc != nil {
		_ = dc.Close()
	}
	// A write blocked on a peer that stopped reading holds the TLS write
	// lock CloseWrite needs: the deadline releases it (the connection is
	// done with writes anyway).
	_ = c.ctrl.SetWriteDeadline(time.Now())
	_ = c.ctrl.CloseWrite()
	time.AfterFunc(time.Second, func() { _ = c.ctrl.Close() })
}

// fail ends the connection on a transport error or a peer's close.
func (c *tcpConn) fail(cause error) {
	c.in.setErr(cause)
	c.cancel(cause)
	c.closeOnce.Do(c.shutdown)
}

func frame(kind byte, p []byte) []byte {
	b := make([]byte, 5+len(p))
	b[0] = kind
	binary.BigEndian.PutUint32(b[1:5], uint32(len(p))) //nolint:gosec // ≤ maxFramePayload (or a close message), never near 4 GiB
	copy(b[5:], p)
	return b
}

// writeLoop owns writes to the control connection. No deadline is ever
// set on it: after a timed-out write a TLS connection is unusable, and a
// cancel must still be able to send its close.
func (c *tcpConn) writeLoop() {
	defer close(c.wdone)
	for {
		select {
		case f := <-c.out:
			_, err := c.ctrl.Write(f.b)
			if f.done != nil {
				f.done <- err
			}
			if err != nil {
				c.fail(fmt.Errorf("quicconn: tcp control write: %w", err))
				return
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// send queues a frame and waits until it is written, the deadline passes
// or the connection ends.
func (c *tcpConn) send(b []byte, deadline time.Time) error {
	var timer <-chan time.Time
	if !deadline.IsZero() {
		d := time.Until(deadline)
		if d <= 0 {
			return os.ErrDeadlineExceeded
		}
		t := time.NewTimer(d)
		defer t.Stop()
		timer = t.C
	}
	done := make(chan error, 1)
	select {
	case c.out <- outFrame{b: b, done: done}:
	case <-timer:
		return os.ErrDeadlineExceeded
	case <-c.ctx.Done():
		return context.Cause(c.ctx)
	}
	select {
	case err := <-done:
		return err
	case <-timer:
		return os.ErrDeadlineExceeded
	case <-c.ctx.Done():
		return context.Cause(c.ctx)
	}
}

// readLoop parses the peer's control frames into c.in. A connection the
// peer stops pinging for the idle timeout is dead, as QUIC's would be.
func (c *tcpConn) readLoop() {
	idle := QuicConfig().MaxIdleTimeout
	br := bufio.NewReader(c.ctrl)
	for {
		_ = c.ctrl.SetReadDeadline(time.Now().Add(idle))
		var hdr [5]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if c.ctx.Err() == nil {
				c.fail(fmt.Errorf("%w: tcp control: %w", fserrors.ErrTransientFailure, err))
			}
			return
		}
		n := binary.BigEndian.Uint32(hdr[1:])
		if n > maxFramePayload+8 {
			c.fail(fmt.Errorf("%w: control frame of %d bytes", errTCPProtocol, n))
			return
		}
		p := make([]byte, n)
		if _, err := io.ReadFull(br, p); err != nil {
			c.fail(fmt.Errorf("%w: tcp control: %w", fserrors.ErrTransientFailure, err))
			return
		}
		switch hdr[0] {
		case frameData:
			if !c.in.push(p, c.ctx.Done()) {
				return
			}
		case frameFIN:
			c.in.setErr(io.EOF)
		case framePing:
		case frameClose:
			if len(p) < 8 {
				c.fail(fmt.Errorf("%w: short close frame", errTCPProtocol))
				return
			}
			c.fail(&quic.ApplicationError{
				Remote:       true,
				ErrorCode:    quic.ApplicationErrorCode(binary.BigEndian.Uint64(p)),
				ErrorMessage: string(p[8:]),
			})
			return
		default:
			c.fail(fmt.Errorf("%w: control frame kind %d", errTCPProtocol, hdr[0]))
			return
		}
	}
}

// pingLoop keeps an idle but live connection (a receiver at a prompt)
// from timing out on the peer, like QUIC's keepalive.
func (c *tcpConn) pingLoop() {
	t := time.NewTicker(QuicConfig().KeepAlivePeriod)
	defer t.Stop()
	ping := frame(framePing, nil)
	for {
		select {
		case <-t.C:
			select {
			case c.out <- outFrame{b: ping}:
			default: // the writer is busy: it is sending anyway
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// ctrlIn buffers control stream bytes for Read, with Read's deadline
// kept here rather than on the TLS connection.
type ctrlIn struct {
	mu    sync.Mutex
	buf   []byte
	err   error // returned once buf is drained: io.EOF, or the close cause
	dl    time.Time
	wake  chan struct{} // poked on new bytes, an error or a new deadline
	space chan struct{} // poked when Read drains
}

func (in *ctrlIn) init() {
	in.wake = make(chan struct{}, 1)
	in.space = make(chan struct{}, 1)
}

func poke(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// push appends p, waiting while the buffer is full. False when stop
// closed first.
func (in *ctrlIn) push(p []byte, stop <-chan struct{}) bool {
	for {
		in.mu.Lock()
		if len(in.buf)+len(p) <= ctrlBufMax || len(in.buf) == 0 {
			in.buf = append(in.buf, p...)
			in.mu.Unlock()
			poke(in.wake)
			return true
		}
		in.mu.Unlock()
		select {
		case <-in.space:
		case <-stop:
			return false
		}
	}
}

func (in *ctrlIn) setErr(err error) {
	in.mu.Lock()
	if in.err == nil {
		in.err = err
	}
	in.mu.Unlock()
	poke(in.wake)
}

func (in *ctrlIn) setDeadline(t time.Time) {
	in.mu.Lock()
	in.dl = t
	in.mu.Unlock()
	poke(in.wake)
}

func (in *ctrlIn) read(p []byte) (int, error) {
	for {
		in.mu.Lock()
		if len(in.buf) > 0 {
			n := copy(p, in.buf)
			in.buf = in.buf[n:]
			if len(in.buf) == 0 {
				in.buf = nil
			}
			in.mu.Unlock()
			poke(in.space)
			return n, nil
		}
		if in.err != nil {
			err := in.err
			in.mu.Unlock()
			return 0, err
		}
		dl := in.dl
		in.mu.Unlock()
		var timer <-chan time.Time
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			t := time.NewTimer(d)
			timer = t.C
			select {
			case <-in.wake:
			case <-timer:
			}
			t.Stop()
			continue
		}
		<-in.wake
	}
}

// tcpCtrlStream is the control stream of a TCP route connection.
type tcpCtrlStream struct {
	c *tcpConn

	mu  sync.Mutex
	wdl time.Time
	fin bool
}

func (s *tcpCtrlStream) Read(p []byte) (int, error) { return s.c.in.read(p) }

func (s *tcpCtrlStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	dl, fin := s.wdl, s.fin
	s.mu.Unlock()
	if fin {
		return 0, errors.New("quicconn: write on a closed stream")
	}
	written := 0
	for len(p) > 0 {
		n := min(len(p), maxFramePayload)
		if err := s.c.send(frame(frameData, p[:n]), dl); err != nil {
			return written, err
		}
		written += n
		p = p[n:]
	}
	return written, nil
}

// Close ends this side of the stream (QUIC's FIN); reads go on until the
// peer's own end, and the connection stays up for its close code.
func (s *tcpCtrlStream) Close() error {
	s.mu.Lock()
	if s.fin {
		s.mu.Unlock()
		return nil
	}
	s.fin = true
	dl := s.wdl
	s.mu.Unlock()
	// Queued behind earlier writes, so it can wait; the write deadline
	// bounds it like any write (a cancel sets one in the past).
	var timer <-chan time.Time
	if !dl.IsZero() {
		t := time.NewTimer(time.Until(dl))
		defer t.Stop()
		timer = t.C
	}
	select {
	case s.c.out <- outFrame{b: frame(frameFIN, nil)}:
	case <-s.c.ctx.Done():
	case <-timer:
		return os.ErrDeadlineExceeded
	}
	return nil
}

func (s *tcpCtrlStream) SetReadDeadline(t time.Time) error {
	s.c.in.setDeadline(t)
	return nil
}

func (s *tcpCtrlStream) SetWriteDeadline(t time.Time) error {
	s.mu.Lock()
	s.wdl = t
	s.mu.Unlock()
	return nil
}

// dataErrGrace is how long a failed data connection waits for the
// peer's close, which travels on the control connection.
const dataErrGrace = 250 * time.Millisecond

// dataErr explains a data connection failure the way QUIC would: as the
// peer's close (a receiver that quit, a sender's cancel) when its close
// frame arrives within dataErrGrace, else as a transient failure to retry.
// A local deadline (this side's cancel) returns at once.
func (c *tcpConn) dataErr(err error) error {
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	t := time.NewTimer(dataErrGrace)
	defer t.Stop()
	select {
	case <-c.ctx.Done():
	case <-t.C:
	}
	var appErr *quic.ApplicationError
	if cause := context.Cause(c.ctx); errors.As(cause, &appErr) && appErr.Remote {
		return fmt.Errorf("quicconn: tcp data: %w", appErr)
	}
	return fmt.Errorf("%w: tcp data: %v", fserrors.ErrTransientFailure, err)
}

// tcpDataOut is the sender's data stream: Close ends it (the receiver
// reads EOF), like a QUIC send stream's FIN.
type tcpDataOut struct {
	c    *tls.Conn
	conn *tcpConn
}

func (d tcpDataOut) Read([]byte) (int, error) { return 0, io.EOF }
func (d tcpDataOut) Write(p []byte) (int, error) {
	n, err := d.c.Write(p)
	return n, d.conn.dataErr(err)
}
func (d tcpDataOut) Close() error                       { return d.c.CloseWrite() }
func (d tcpDataOut) SetWriteDeadline(t time.Time) error { return d.c.SetWriteDeadline(t) }

// tcpDataIn is the receiver's data stream: Close stops reading, which
// fails the sender's writes, like QUIC's CancelRead.
type tcpDataIn struct {
	c    *tls.Conn
	conn *tcpConn
}

func (d tcpDataIn) Read(p []byte) (int, error) {
	n, err := d.c.Read(p)
	return n, d.conn.dataErr(err)
}
func (d tcpDataIn) Write([]byte) (int, error) {
	return 0, errors.New("quicconn: stream is receive-only")
}
func (d tcpDataIn) Close() error                      { return d.c.Close() }
func (d tcpDataIn) SetReadDeadline(t time.Time) error { return d.c.SetReadDeadline(t) }

// dataID names a session's data connection: both peers derive it from
// the session binding once the PAKE has succeeded.
func dataID(binding []byte) []byte {
	m := hmac.New(sha256.New, binding)
	m.Write([]byte(tagLabelData + "/id"))
	return m.Sum(nil)
}

// dataTag proves a data connection belongs to the session and was not
// relayed: the binding only the two peers hold, over this connection's
// own exporter, separated by role.
func dataTag(binding []byte, st tls.ConnectionState, r role) ([]byte, error) {
	exp, err := st.ExportKeyingMaterial(exporterLabel, nil, exporterLen)
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, binding)
	m.Write([]byte(tagLabelData))
	m.Write(exp)
	m.Write([]byte{byte(r)})
	return m.Sum(nil), nil
}

// ServeTCP accepts the TCP route on ln alongside the QUIC listener: the
// same secret, peer filter, handshake cap and failure penalty, and the
// connections it authenticates come out of the same Accept. Close stops
// it too.
func (l *Listener) ServeTCP(ln net.Listener) error {
	tlsCfg, err := SenderTLSConfig()
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.tcp = ln
	l.pending = make(map[string]chan *tls.Conn)
	l.mu.Unlock()
	go l.tcpAcceptLoop(ln, tcpTLS(tlsCfg))
	return nil
}

func (l *Listener) tcpAcceptLoop(ln net.Listener, cfg *tls.Config) {
	pre := make(chan struct{}, maxPreTLS)
	dataSem := make(chan struct{}, maxPendingData)
	for {
		nc, err := ln.Accept()
		if err != nil {
			select {
			case <-l.done:
				return
			default:
			}
			if errors.Is(err, net.ErrClosed) {
				return // QUIC keeps serving
			}
			// Out of file descriptors and the like: wait, then go on.
			select {
			case <-time.After(100 * time.Millisecond):
				continue
			case <-l.done:
				return
			}
		}
		if l.allow != nil && !l.allow(nc.RemoteAddr()) {
			_ = nc.Close()
			continue
		}
		select {
		case pre <- struct{}{}:
		default:
			_ = nc.Close() // too many connects still in their handshake
			continue
		}
		go l.serveTCPConn(nc, cfg, pre, dataSem)
	}
}

// serveTCPConn handshakes one TCP route connection and routes it by the
// receiver's first byte.
func (l *Listener) serveTCPConn(nc net.Conn, cfg *tls.Config, pre, dataSem chan struct{}) {
	// The TLS handshake and the hello take milliseconds on a local
	// network; a connection that sits silent through them is let go.
	_ = nc.SetDeadline(time.Now().Add(tcpTLSTimeout))
	tc := tls.Server(nc, cfg)
	var hello [1]byte
	err := tc.Handshake()
	if err == nil {
		_, err = io.ReadFull(tc, hello[:])
	}
	// The pre-TLS slot is held until a handshake or data slot is, so
	// connections waiting out the failure penalty stay counted.
	releasePre := sync.OnceFunc(func() { <-pre })
	defer releasePre()
	if err != nil {
		_ = tc.Close()
		return
	}
	sem := l.sem
	switch hello[0] {
	case tcpRoleControl:
		if !l.pause() {
			_ = tc.Close()
			return
		}
		select {
		case sem <- struct{}{}:
		default:
			_ = tc.Close() // busy, as QUIC's closeBusy
			return
		}
		releasePre()
		defer func() { <-sem }()
		_ = nc.SetDeadline(time.Time{}) // the conn's read loop takes over
		res, err := l.tcpSenderHandshake(tc)
		l.handshakeDone(err)
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
	case tcpRoleData:
		select {
		case dataSem <- struct{}{}:
		default:
			_ = tc.Close()
			return
		}
		releasePre()
		defer func() { <-dataSem }()
		_ = nc.SetDeadline(time.Now().Add(tcpTLSTimeout))
		var id [sha256.Size]byte
		if _, err := io.ReadFull(tc, id[:]); err != nil {
			_ = tc.Close()
			return
		}
		ch := l.awaitPending(string(id[:]))
		if ch == nil {
			_ = tc.Close()
			return
		}
		select {
		case ch <- tc: // the control side verifies the tags
		default:
			_ = tc.Close()
		}
	default:
		_ = tc.Close()
	}
}

// awaitPending returns the channel a session's control side waits on for
// the data connection with id, giving the session pendingWait to appear.
func (l *Listener) awaitPending(id string) chan *tls.Conn {
	deadline := time.Now().Add(pendingWait)
	for {
		l.mu.Lock()
		ch := l.pending[id]
		l.mu.Unlock()
		if ch != nil || time.Now().After(deadline) {
			return ch
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-l.done:
			return nil
		}
	}
}

// tcpSenderHandshake runs the PAKE on a control connection, then waits
// for the receiver's data connection and checks both tags.
func (l *Listener) tcpSenderHandshake(tc *tls.Conn) (res *AcceptResult, err error) {
	c := newTCPConn(tc)
	ctrl := &tcpCtrlStream{c: c}
	defer func() {
		if err != nil {
			_ = c.CloseWithError(closeHandshakeFailed, "handshake failed")
		}
	}()
	_ = ctrl.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	binding, err := authenticatePeer(tc.ConnectionState(), ctrl, l.currentSecret(), roleSender)
	if err != nil {
		return nil, err
	}
	_ = ctrl.SetReadDeadline(time.Time{})

	id := string(dataID(binding))
	ch := make(chan *tls.Conn, 1)
	l.mu.Lock()
	l.pending[id] = ch
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.pending, id)
		l.mu.Unlock()
	}()
	var dc *tls.Conn
	select {
	case dc = <-ch:
	case <-time.After(HandshakeTimeout):
		return nil, fmt.Errorf("%w: it never arrived", ErrTCPData)
	case <-c.ctx.Done():
		return nil, context.Cause(c.ctx)
	case <-l.done:
		return nil, l.err
	}
	if err := checkDataTags(dc, binding, roleSender); err != nil {
		_ = dc.Close()
		return nil, err
	}
	c.setData(dc)
	return &AcceptResult{
		Conn: c,
		Streams: transfer.Streams{
			Control: ctrl,
			Data:    tcpDataOut{c: dc, conn: c},
			Binding: binding,
		},
	}, nil
}

// checkDataTags exchanges tags on a data connection: the receiver's comes
// first (it already sent its id), the sender's answers. me is this side.
func checkDataTags(dc *tls.Conn, binding []byte, me role) error {
	_ = dc.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer func() { _ = dc.SetDeadline(time.Time{}) }()
	st := dc.ConnectionState()
	mine, err := dataTag(binding, st, me)
	if err != nil {
		return err
	}
	theirs, err := dataTag(binding, st, otherRole(me))
	if err != nil {
		return err
	}
	var got [sha256.Size]byte
	if me == roleReceiver {
		if _, err := dc.Write(mine); err != nil {
			return fmt.Errorf("%w: tag: %v", ErrTCPData, err)
		}
	}
	if _, err := io.ReadFull(dc, got[:]); err != nil {
		return fmt.Errorf("%w: tag: %v", ErrTCPData, err)
	}
	if subtle.ConstantTimeCompare(got[:], theirs) != 1 {
		return fmt.Errorf("%w: it is not this session's", ErrTCPData)
	}
	if me == roleSender {
		if _, err := dc.Write(mine); err != nil {
			return fmt.Errorf("%w: tag: %v", ErrTCPData, err)
		}
	}
	return nil
}

// DialTCP is DialTimeout over the TCP route: connectTimeout bounds the
// TCP connect, so a sender without it (an older release, a firewall that
// drops TCP) costs little before the caller falls back to QUIC.
func DialTCP(ctx context.Context, addr, secret string, connectTimeout time.Duration) (res *AcceptResult, err error) {
	cfg := tcpTLS(ReceiverTLSConfig())
	dial := func(role byte) (*tls.Conn, error) {
		d := net.Dialer{Timeout: connectTimeout}
		nc, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("quicconn: tcp dial: %w", err)
		}
		tc := tls.Client(nc, cfg)
		hctx, cancel := context.WithTimeout(ctx, tcpTLSTimeout)
		defer cancel()
		if err := tc.HandshakeContext(hctx); err != nil {
			_ = tc.Close()
			return nil, fmt.Errorf("quicconn: tcp tls: %w", err)
		}
		if _, err := tc.Write([]byte{role}); err != nil {
			_ = tc.Close()
			return nil, fmt.Errorf("quicconn: tcp hello: %w", err)
		}
		return tc, nil
	}
	ctrlTLS, err := dial(tcpRoleControl)
	if err != nil {
		return nil, err
	}
	c := newTCPConn(ctrlTLS)
	ctrl := &tcpCtrlStream{c: c}
	defer func() {
		if err != nil {
			_ = c.CloseWithError(closeHandshakeFailed, "handshake failed")
		}
	}()
	_ = ctrl.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	binding, err := authenticatePeer(ctrlTLS.ConnectionState(), ctrl, secret, roleReceiver)
	if err != nil {
		return nil, err
	}
	_ = ctrl.SetReadDeadline(time.Time{})

	dc, err := dial(tcpRoleData)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTCPData, err)
	}
	if _, err := dc.Write(dataID(binding)); err != nil {
		_ = dc.Close()
		return nil, fmt.Errorf("%w: id: %v", ErrTCPData, err)
	}
	if err := checkDataTags(dc, binding, roleReceiver); err != nil {
		_ = dc.Close()
		return nil, err
	}
	c.setData(dc)
	return &AcceptResult{
		Conn: c,
		Streams: transfer.Streams{
			Control: ctrl,
			Data:    tcpDataIn{c: dc, conn: c},
			Binding: binding,
		},
	}, nil
}
