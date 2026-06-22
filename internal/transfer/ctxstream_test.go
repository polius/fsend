package transfer

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// blockingStream models a QUIC stream whose Read parks until a (past)
// deadline is set, then returns a timeout error — exactly the shape bindCtx
// relies on to interrupt a wedged peer.
type blockingStream struct {
	once    sync.Once
	unblock chan struct{}
}

func newBlockingStream() *blockingStream { return &blockingStream{unblock: make(chan struct{})} }

func (b *blockingStream) Read([]byte) (int, error) {
	<-b.unblock
	return 0, os.ErrDeadlineExceeded
}
func (b *blockingStream) Write(p []byte) (int, error) { return len(p), nil }
func (b *blockingStream) Close() error                { return nil }
func (b *blockingStream) SetReadDeadline(time.Time) error {
	b.once.Do(func() { close(b.unblock) })
	return nil
}
func (b *blockingStream) SetWriteDeadline(time.Time) error { return nil }

func TestBindCtx_UnblocksBlockedReadOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &Streams{Control: newBlockingStream(), Data: newBlockingStream()}
	wrapped, stop := bindCtx(ctx, s)
	defer stop()

	errc := make(chan error, 1)
	go func() {
		_, err := wrapped.Data.Read(make([]byte, 8))
		errc <- err
	}()

	cancel() // must interrupt the parked Read and surface ctx.Err()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not unblock within 2s of cancel")
	}
}

// errStream returns a fixed error on Read so we can check ctxConn's error
// remapping in both ctx states.
type errStream struct{ err error }

func (e errStream) Read([]byte) (int, error)    { return 0, e.err }
func (e errStream) Write(p []byte) (int, error) { return len(p), nil }
func (e errStream) Close() error                { return nil }

func TestCtxConn_PassesThroughErrorWhenNotCancelled(t *testing.T) {
	c := &ctxConn{inner: errStream{err: io.EOF}, ctx: context.Background()}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("want io.EOF passed through, got %v", err)
	}
}

func TestCtxConn_RemapsErrorWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &ctxConn{inner: errStream{err: io.ErrUnexpectedEOF}, ctx: ctx}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}
