package transfer

import (
	"context"
	"io"
	"time"
)

// bindCtx makes a Streams pair promptly cancellable.
//
// QUIC reads/writes take no context, and the handshake deadline is cleared
// once auth completes, so a blocked Read/Write only returns when the peer
// responds or the connection's idle timeout (~30s) fires. Without this, a
// Ctrl-C while the data loop is parked on a wedged peer or a stalled disk
// isn't observed for up to that long.
//
// On cancel the watcher sets a past deadline on the data stream (both
// directions) and the control read side, unblocking the parked call with a
// deadline error; the returned ctxConn then reports it as ctx.Err() so the
// loops surface a clean cancellation. The control *write* side is left alone
// so the sender's best-effort cancel notification (notifyCancel) still
// reaches the peer.
//
// Streams that don't expose deadlines (the io.Pipe pair used in tests) are
// simply not interrupted — behavior there is unchanged from the per-loop
// ctx.Err() checks. The returned stop func ends the watcher; callers defer it.
func bindCtx(ctx context.Context, s *Streams) (*Streams, func()) {
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			past := time.Now()
			setReadDeadline(s.Control, past)
			setReadDeadline(s.Data, past)
			setWriteDeadline(s.Data, past)
		case <-done:
		}
	}()
	wrapped := &Streams{
		Control: &ctxConn{inner: s.Control, ctx: ctx},
		Data:    &ctxConn{inner: s.Data, ctx: ctx},
	}
	return wrapped, func() { close(done) }
}

// ctxConn delegates to an underlying stream but reports ctx.Err() in place of
// the deadline/IO error that bindCtx's watcher induces on cancellation, so a
// cancelled transfer surfaces as context.Canceled rather than a transient
// read/write failure that the retry layer might misclassify.
type ctxConn struct {
	inner io.ReadWriteCloser
	ctx   context.Context
}

func (c *ctxConn) Read(p []byte) (int, error) {
	n, err := c.inner.Read(p)
	return n, c.cancelErr(err)
}

func (c *ctxConn) Write(p []byte) (int, error) {
	n, err := c.inner.Write(p)
	return n, c.cancelErr(err)
}

func (c *ctxConn) Close() error { return c.inner.Close() }

func (c *ctxConn) cancelErr(err error) error {
	if err != nil && c.ctx.Err() != nil {
		return c.ctx.Err()
	}
	return err
}

type readDeadliner interface{ SetReadDeadline(time.Time) error }
type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

func setReadDeadline(rw io.ReadWriteCloser, t time.Time) {
	if d, ok := rw.(readDeadliner); ok {
		_ = d.SetReadDeadline(t)
	}
}

func setWriteDeadline(rw io.ReadWriteCloser, t time.Time) {
	if d, ok := rw.(writeDeadliner); ok {
		_ = d.SetWriteDeadline(t)
	}
}
