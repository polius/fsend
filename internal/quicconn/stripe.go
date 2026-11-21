package quicconn

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/polius/fsend/internal/fserrors"
)

// Striping spreads one transfer's data stream over several QUIC
// connections ("lanes") on the same path, each with its own congestion
// window. A loss-based congestion controller backs off on every lost
// packet, so one connection on a path with random loss tops out far below
// the link (about 1.7 MB/s at 0.1% loss and 30 ms); lanes add up. The
// transfer engine still sees one ordered stream: the sender cuts it into
// numbered stripes that go out on whichever lane has room (a lane slowed
// by a loss takes fewer), and the receiver puts them back in order.
//
// Each lane's data stream, after the priming byte, is a run of frames:
//
//	[seq u64][len u32][len bytes]
//
// and the first lane ends with a frame of seq endSeq whose 8 bytes are the
// number of stripes sent, so the receiver knows the stream is complete
// whatever lane the last stripe took.

// stripeSize is the payload of one stripe: small enough that lanes share
// the work evenly and the reorder window stays a few MiB, large enough
// that the 12-byte header is noise.
const stripeSize = 64 << 10

const stripeHeader = 12

// endSeq marks the frame that ends the stream with the stripe count.
const endSeq = math.MaxUint64

// reorderWindow is how many stripes past the next one in order the
// receiver holds (2 MiB). A lane whose next stripe is further ahead
// waits, and stops reading: its flow control then holds the sender back.
const reorderWindow = 32

// stripeQueue is how many full stripes wait for a lane on the sender.
const stripeQueue = 2

var stripeFree = make(chan []byte, 64)

func getStripe() []byte {
	select {
	case b := <-stripeFree:
		return b
	default:
		return make([]byte, stripeHeader+stripeSize)
	}
}

func putStripe(b []byte) {
	if cap(b) != stripeHeader+stripeSize {
		return
	}
	select {
	case stripeFree <- b[:stripeHeader+stripeSize]:
	default:
	}
}

// laneOut is one lane's send stream. CancelWrite abandons it (and unblocks
// a Write in progress), as an abort.
type laneOut interface {
	io.WriteCloser
	SetWriteDeadline(time.Time) error
	CancelWrite()
}

// stripeOut is the sender's data stream over lanes.
type stripeOut struct {
	queue chan []byte // full stripes (header filled in) waiting for a lane

	mu      sync.Mutex
	cur     []byte // the stripe being filled; its payload is cur[stripeHeader:fill]
	fill    int
	seq     uint64 // stripes handed to the queue
	err     error  // first lane failure; the stream is broken
	closed  bool
	lanes   []laneOut
	first   laneOut // carries the end frame
	dl      time.Time
	onClose func() // unregisters the session from the listener
	wg      sync.WaitGroup

	wmu   sync.Mutex    // held by Write, and by Close while it flushes
	abort chan struct{} // closed by a Close that finds a Write in progress
}

func newStripeOut(first laneOut) *stripeOut {
	o := &stripeOut{queue: make(chan []byte, stripeQueue), first: first, abort: make(chan struct{})}
	o.addLane(first)
	return o
}

// addLane starts sending stripes on another lane. After Close it only
// ends the lane, so the receiver isn't left waiting on it.
func (o *stripeOut) addLane(l laneOut) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		_ = l.Close()
		return
	}
	if !o.dl.IsZero() {
		_ = l.SetWriteDeadline(o.dl)
	}
	o.lanes = append(o.lanes, l)
	o.wg.Add(1)
	go o.run(l)
}

func (o *stripeOut) run(l laneOut) {
	defer o.wg.Done()
	for b := range o.queue {
		if o.failed() == nil {
			if _, err := l.Write(b); err != nil {
				o.fail(err)
			}
		}
		putStripe(b)
	}
}

func (o *stripeOut) failed() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *stripeOut) fail(err error) {
	o.mu.Lock()
	if o.err == nil {
		o.err = err
	}
	o.mu.Unlock()
}

func (o *stripeOut) Read([]byte) (int, error) { return 0, io.EOF }

// Write cuts p into stripes and queues them, waiting while every lane is
// busy (the network's pace).
func (o *stripeOut) Write(p []byte) (int, error) {
	o.wmu.Lock()
	defer o.wmu.Unlock()
	if o.isClosed() {
		return 0, io.ErrClosedPipe
	}
	n := 0
	for len(p) > 0 {
		if err := o.failed(); err != nil {
			return n, err
		}
		if o.cur == nil {
			o.cur, o.fill = getStripe(), stripeHeader
		}
		k := copy(o.cur[o.fill:], p)
		o.fill += k
		p = p[k:]
		n += k
		if o.fill == len(o.cur) {
			if err := o.push(); err != nil {
				return n, err
			}
		}
	}
	// What is left goes now, as a short stripe: the engine writes whole
	// frames (a chunk's header, its payload) and may not write again until
	// the receiver has answered them, so nothing may wait for more bytes.
	if o.cur != nil && o.fill > stripeHeader {
		if err := o.push(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// push numbers the current stripe and queues it for a lane.
func (o *stripeOut) push() error {
	b := o.cur[:o.fill]
	o.cur = nil
	binary.BigEndian.PutUint64(b[0:8], o.seq)
	binary.BigEndian.PutUint32(b[8:12], uint32(len(b)-stripeHeader)) //nolint:gosec // ≤ stripeSize
	o.seq++
	o.mu.Lock()
	dl := o.dl
	o.mu.Unlock()
	var expire <-chan time.Time
	if !dl.IsZero() {
		t := time.NewTimer(time.Until(dl))
		defer t.Stop()
		expire = t.C
	}
	select {
	case o.queue <- b:
		return nil
	case <-expire:
		putStripe(b)
		return os.ErrDeadlineExceeded
	case <-o.abort:
		putStripe(b)
		return io.ErrClosedPipe
	}
}

func (o *stripeOut) isClosed() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.closed
}

// Close sends what is buffered, waits for the lanes to hand everything to
// QUIC, sends the stripe count on the first lane and ends every lane. A
// Close while a Write is blocked (a cancel from another goroutine) aborts
// instead: the receiver then sees the stream end short of the count.
func (o *stripeOut) Close() error {
	aborted := false
	if !o.wmu.TryLock() {
		aborted = true
		o.mu.Lock()
		if !o.closed {
			select {
			case <-o.abort:
			default:
				close(o.abort)
			}
		}
		o.mu.Unlock()
		o.wmu.Lock()
	}
	defer o.wmu.Unlock()
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return o.failed()
	}
	o.mu.Unlock()
	if !aborted && o.cur != nil && o.fill > stripeHeader && o.failed() == nil {
		_ = o.push()
	}
	o.mu.Lock()
	o.closed = true
	lanes := o.lanes
	o.mu.Unlock()
	if aborted {
		for _, l := range lanes {
			l.CancelWrite()
		}
	}
	close(o.queue)
	o.wg.Wait()
	if o.cur != nil {
		putStripe(o.cur)
		o.cur = nil
	}
	if !aborted && o.failed() == nil {
		var end [stripeHeader + 8]byte
		binary.BigEndian.PutUint64(end[0:8], endSeq)
		binary.BigEndian.PutUint32(end[8:12], 8)
		binary.BigEndian.PutUint64(end[12:], o.seq)
		if _, err := o.first.Write(end[:]); err != nil {
			o.fail(err)
		}
	}
	if !aborted {
		for _, l := range lanes {
			_ = l.Close()
		}
	}
	if o.onClose != nil {
		o.onClose()
	}
	if aborted && o.failed() == nil {
		return io.ErrClosedPipe
	}
	return o.failed()
}

// SetWriteDeadline applies to every lane and to waiting for one.
func (o *stripeOut) SetWriteDeadline(t time.Time) error {
	o.mu.Lock()
	o.dl = t
	lanes := o.lanes
	o.mu.Unlock()
	for _, l := range lanes {
		_ = l.SetWriteDeadline(t)
	}
	return nil
}

// laneIn is one lane's receive stream.
type laneIn interface {
	io.Reader
	CancelRead()
	SetReadDeadline(time.Time) error
}

// stripeIn is the receiver's data stream over lanes.
type stripeIn struct {
	mu        sync.Mutex
	cond      *sync.Cond
	pending   map[uint64][]byte // stripes arrived ahead of next, by seq
	next      uint64            // the next stripe Read returns
	cur       []byte            // the stripe being read (a pooled buffer)
	off, end  int               // its unread bytes are cur[off:end]
	total     uint64            // stripes sent, once the end frame arrived
	haveTotal bool
	live      int // lanes still reading
	attaching int // lanes being set up
	lanes     []laneIn
	err       error
	closed    bool
	expired   bool
	dlTimer   *time.Timer
}

func newStripeIn(first laneIn) *stripeIn {
	in := &stripeIn{pending: make(map[uint64][]byte)}
	in.cond = sync.NewCond(&in.mu)
	in.addLane(first)
	return in
}

// expectLane notes a lane being set up, so a stream whose other lanes
// have all ended keeps waiting for it; laneFailed is its end without one.
func (in *stripeIn) expectLane() {
	in.mu.Lock()
	in.attaching++
	in.mu.Unlock()
}

func (in *stripeIn) laneFailed() {
	in.mu.Lock()
	in.attaching--
	in.cond.Broadcast()
	in.mu.Unlock()
}

// attachLane is addLane for a lane announced with expectLane.
func (in *stripeIn) attachLane(l laneIn) {
	in.mu.Lock()
	in.attaching--
	in.mu.Unlock()
	in.addLane(l)
}

func (in *stripeIn) addLane(l laneIn) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		l.CancelRead()
		return
	}
	in.lanes = append(in.lanes, l)
	in.live++
	go in.run(l)
}

func (in *stripeIn) run(l laneIn) {
	defer func() {
		in.mu.Lock()
		in.live--
		in.cond.Broadcast()
		in.mu.Unlock()
	}()
	var hdr [stripeHeader]byte
	for {
		if _, err := io.ReadFull(l, hdr[:]); err != nil {
			if !errors.Is(err, io.EOF) {
				in.fail(err)
			}
			return
		}
		seq := binary.BigEndian.Uint64(hdr[0:8])
		n := int(binary.BigEndian.Uint32(hdr[8:12]))
		if seq == endSeq {
			var t [8]byte
			if n != 8 {
				in.fail(fmt.Errorf("%w: stripe end frame of %d bytes", fserrors.ErrProtocolError, n))
				return
			}
			if _, err := io.ReadFull(l, t[:]); err != nil {
				in.fail(err)
				return
			}
			in.mu.Lock()
			in.total, in.haveTotal = binary.BigEndian.Uint64(t[:]), true
			in.cond.Broadcast()
			in.mu.Unlock()
			continue
		}
		if n == 0 || n > stripeSize {
			in.fail(fmt.Errorf("%w: stripe of %d bytes", fserrors.ErrProtocolError, n))
			return
		}
		b := getStripe()
		if _, err := io.ReadFull(l, b[:n]); err != nil {
			putStripe(b)
			in.fail(err)
			return
		}
		in.mu.Lock()
		for !in.closed && in.err == nil && seq >= in.next+reorderWindow {
			in.cond.Wait()
		}
		_, dup := in.pending[seq]
		switch {
		case in.closed || in.err != nil:
			in.mu.Unlock()
			putStripe(b)
			return
		case seq < in.next || dup:
			in.mu.Unlock()
			putStripe(b)
			in.fail(fmt.Errorf("%w: stripe %d repeated", fserrors.ErrProtocolError, seq))
			return
		}
		in.pending[seq] = b[:n]
		in.cond.Broadcast()
		in.mu.Unlock()
	}
}

func (in *stripeIn) fail(err error) {
	in.mu.Lock()
	if in.err == nil && !in.closed {
		in.err = err
	}
	in.cond.Broadcast()
	in.mu.Unlock()
}

// Read returns the stream's bytes in order.
func (in *stripeIn) Read(p []byte) (int, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for {
		if in.cur != nil {
			n := copy(p, in.cur[in.off:in.end])
			in.off += n
			if in.off == in.end {
				putStripe(in.cur)
				in.cur = nil
			}
			return n, nil
		}
		if b, ok := in.pending[in.next]; ok {
			delete(in.pending, in.next)
			in.next++
			in.cur, in.off, in.end = b[:cap(b)], 0, len(b)
			in.cond.Broadcast() // the window moved
			continue
		}
		switch {
		case in.closed:
			return 0, io.ErrClosedPipe
		case in.err != nil:
			return 0, in.err
		case in.haveTotal && in.next == in.total:
			return 0, io.EOF
		case in.expired:
			return 0, os.ErrDeadlineExceeded
		case in.live == 0 && in.attaching == 0:
			return 0, fmt.Errorf("striped stream: %w", io.ErrUnexpectedEOF)
		}
		in.cond.Wait()
	}
}

func (in *stripeIn) Write([]byte) (int, error) {
	return 0, errors.New("quicconn: stream is receive-only")
}

// Close stops reading every lane, as a QUIC receive stream's CancelRead.
func (in *stripeIn) Close() error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil
	}
	in.closed = true
	for _, l := range in.lanes {
		l.CancelRead()
	}
	for seq, b := range in.pending {
		putStripe(b[:cap(b)])
		delete(in.pending, seq)
	}
	if in.cur != nil {
		putStripe(in.cur)
		in.cur = nil
	}
	in.cond.Broadcast()
	return nil
}

// SetReadDeadline bounds Read; a lane that is waiting is unblocked too.
func (in *stripeIn) SetReadDeadline(t time.Time) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.dlTimer != nil {
		in.dlTimer.Stop()
		in.dlTimer = nil
	}
	in.expired = false
	if t.IsZero() {
		return nil
	}
	d := time.Until(t)
	if d <= 0 {
		in.expired = true
		in.cond.Broadcast()
		return nil
	}
	in.dlTimer = time.AfterFunc(d, func() {
		in.mu.Lock()
		in.expired = true
		in.cond.Broadcast()
		in.mu.Unlock()
	})
	return nil
}
