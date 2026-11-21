package relay

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/net/ipv4"
)

// batchConn reads and writes several datagrams per system call (see
// newBatchConn). ipv4.Message and ipv6.Message are the same type.
type batchConn interface {
	ReadBatch(ms []ipv4.Message, flags int) (int, error)
	WriteBatch(ms []ipv4.Message, flags int) (int, error)
}

// batchSize is how many datagrams one system call reads or writes. A
// batch is only as full as the socket's queue: under light load each call
// still returns a single datagram, at no extra latency.
const batchSize = 32

// outBatch collects the datagrams a read batch forwards, to send them in
// one call. Each one points into the read buffers, which stay untouched
// until the batch is flushed.
type outBatch struct {
	ms []ipv4.Message
	n  int
}

func newOutBatch() *outBatch {
	o := &outBatch{ms: make([]ipv4.Message, batchSize)}
	for i := range o.ms {
		o.ms[i].Buffers = make([][]byte, 1)
	}
	return o
}

// add queues b for to; false when the batch is full.
func (o *outBatch) add(b []byte, to *net.UDPAddr) bool {
	if o.n == len(o.ms) {
		return false
	}
	m := &o.ms[o.n]
	m.Buffers[0], m.Addr = b, to
	o.n++
	return true
}

// flush writes the queued datagrams. A datagram the kernel refuses (an
// unreachable peer) is dropped, like a failed WriteTo: the rest still go.
func (s *Server) flush(bc batchConn, o *outBatch) {
	ms := o.ms[:o.n]
	for len(ms) > 0 {
		k, err := bc.WriteBatch(ms, 0)
		if err != nil {
			s.logger.Debug("relay: write failed", "err", err)
			k = max(k, 1)
		}
		ms = ms[k:]
	}
	for i := range o.ms[:o.n] {
		o.ms[i].Buffers[0], o.ms[i].Addr = nil, nil
	}
	o.n = 0
}

// readBatchLoop is readLoop with recvmmsg and sendmmsg: it reads up to
// batchSize datagrams, handles each, and writes what they forward in one
// call before reading again.
func (s *Server) readBatchLoop(ctx context.Context, bc batchConn) error {
	in := make([]ipv4.Message, batchSize)
	for i := range in {
		// One byte over MaxDatagram, as in readLoop.
		in[i].Buffers = [][]byte{make([]byte, MaxDatagram+1)}
	}
	out := newOutBatch()
	readErrs := 0
	for {
		n, err := bc.ReadBatch(in, 0)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			readErrs++
			if errors.Is(err, net.ErrClosed) || readErrs > maxReadErrors {
				s.healthy.Store(false)
				return fmt.Errorf("relay: read: %w", err)
			}
			s.logger.Debug("relay: transient read error", "err", err)
			time.Sleep(readErrBackoff)
			continue
		}
		readErrs = 0
		for i := range in[:n] {
			m := &in[i]
			if m.N > MaxDatagram {
				s.oversize.Add(1)
				continue
			}
			ua, ok := m.Addr.(*net.UDPAddr)
			if !ok {
				continue
			}
			s.handleSafe(m.Buffers[0][:m.N], endpoint{udp: ua}, out)
		}
		s.flush(bc, out)
	}
}
