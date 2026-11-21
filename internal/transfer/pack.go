package transfer

import (
	"fmt"
	"io"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// chunkPacker streams file bytes onto the data stream, packing several whole
// small files into one chunk (so they compress together) while slicing large
// files across chunks. Full chunks are compressed and written by a separate
// goroutine, so reading the next bytes overlaps compression and the network.
type chunkPacker struct {
	buf     []byte
	segs    []wire.Segment
	restart map[uint32]bool // next segment of this file carries SegFlagRestart

	out  chan packed
	done chan struct{}
	mu   sync.Mutex
	err  error // first write error, surfaced on the next append
}

type packed struct {
	segs    []wire.Segment
	buf     []byte
	release func() // returns buf once written; nil: buf is the packer's own
}

// free returns the chunk's buffer once it is written (or dropped).
func (c packed) free() {
	if c.release != nil {
		c.release()
		return
	}
	putBlock(c.buf)
}

// encoderPool holds the zstd encoders every transfer in this process
// compresses its chunks with, each a single-worker encoder used by one
// chunk at a time. They live for the process: one keeps a history buffer
// and match tables (a few MiB), so an encoder per transfer allocated that
// for every transfer, and again for every retry. They are made on first
// use, up to packWorkers: data that doesn't compress only ever wakes one
// (compressBackoff probes with a single chunk), so a send of photos or
// archives doesn't pay for four. A chunk is at most 1 MiB, so that is all
// the window it can use: the bytes on the wire are the same as with the
// default window.
type encoderPool struct {
	mu   sync.Mutex
	cond *sync.Cond // signalled when an encoder is returned
	idle []*zstd.Encoder
	made int
}

var chunkEncoders = newEncoderPool()

func newEncoderPool() *encoderPool {
	p := &encoderPool{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// get returns an idle encoder, makes one while fewer than packWorkers
// exist, or waits for one to come back (only when transfers in one process
// overlap: a transfer runs packWorkers workers, each with one encoder).
func (p *encoderPool) get() (*zstd.Encoder, error) {
	p.mu.Lock()
	for len(p.idle) == 0 && p.made >= packWorkers() {
		p.cond.Wait()
	}
	if n := len(p.idle); n > 0 {
		e := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		return e, nil
	}
	p.made++
	p.mu.Unlock()
	e, err := zstd.NewWriter(nil, zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(wire.MaxChunkSize))
	if err != nil {
		p.mu.Lock()
		p.made--
		p.cond.Signal()
		p.mu.Unlock()
		return nil, fmt.Errorf("send: zstd writer: %w", err)
	}
	return e, nil
}

func (p *encoderPool) put(e *zstd.Encoder) {
	p.mu.Lock()
	p.idle = append(p.idle, e)
	p.cond.Signal()
	p.mu.Unlock()
}

// compress returns src compressed into dst, or nil when no encoder could
// be made (the chunk then goes uncompressed).
func (p *encoderPool) compress(src, dst []byte) []byte {
	e, err := p.get()
	if err != nil {
		return nil
	}
	defer p.put(e)
	return e.EncodeAll(src, dst)
}

func newChunkPacker(w io.Writer) (*chunkPacker, error) {
	p := &chunkPacker{buf: getBlock()[:0], out: make(chan packed, 2), done: make(chan struct{})}
	go p.writeLoop(p.out, w, chunkEncoders)
	return p, nil
}

// packWorkers is how many chunks compress at once. One core held text to
// about a third of the engine's speed; a few cores lift it near the
// network's. Four move 657 MB of source code over the local route as fast
// as eight (about 0.95 s on arm64, also on emulated amd64 and a 1 Gbit
// LAN), and each encoder is about 3 MiB: eight held 12–23 MiB more on
// the sender. Two were 30% slower.
func packWorkers() int { return min(runtime.GOMAXPROCS(0), 4) }

// packJob is one chunk on its way through the compression workers.
type packJob struct {
	c      packed
	try    bool   // compress it (false while backing off incompressible data)
	probe  bool   // it is the one chunk finding out whether data compresses
	z      []byte // the compressed bytes, when tried
	shrank bool   // z is worth sending: smaller by more than a tenth
	done   chan struct{}
}

// compressBackoff decides which chunks are worth compressing. After a
// chunk that doesn't shrink, the next ones go uncompressed for a growing
// stretch, so incompressible data doesn't burn cores on zstd. While it is
// not known whether the data compresses (at the start, and after each
// stretch) a single chunk probes it and the next one waits for the answer:
// otherwise every worker would try a chunk of an incompressible file at
// once, each waking an encoder and taking a second buffer. Once a chunk
// shrinks, chunks are tried on every worker. Results arrive as chunks
// finish, not in order: harmless, a chunk's compression is its own flag
// on the wire.
type compressBackoff struct {
	mu            sync.Mutex
	skip, backoff int
	compresses    bool          // the last result shrank: try every chunk
	probing       chan struct{} // closed when the probe's result is in
}

// shouldTry reports whether to compress the next chunk, and whether it is
// a probe whose result the one after it waits for. It may wait for the
// result of the probe in flight; a worker always delivers it.
func (b *compressBackoff) shouldTry() (try, probe bool) {
	b.mu.Lock()
	for b.probing != nil {
		wait := b.probing
		b.mu.Unlock()
		<-wait
		b.mu.Lock()
	}
	defer b.mu.Unlock()
	if b.skip > 0 {
		b.skip--
		return false, false
	}
	if !b.compresses {
		b.probing = make(chan struct{})
		return true, true
	}
	return true, false
}

func (b *compressBackoff) result(shrank, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe && b.probing != nil {
		close(b.probing)
		b.probing = nil
	}
	b.compresses = shrank
	if shrank {
		b.backoff = 0
		return
	}
	b.backoff = min(max(1, b.backoff*2), 64)
	b.skip = b.backoff
}

// writeLoop compresses chunks on packWorkers goroutines (when it pays)
// and writes them in order: reading, compressing and the network overlap,
// and compression uses more than one core.
func (p *chunkPacker) writeLoop(in <-chan packed, w io.Writer, enc *encoderPool) {
	defer close(p.done)
	n := packWorkers()
	jobs := make(chan *packJob, n)
	order := make(chan *packJob, n) // bounds the chunks in flight
	// Chunks sent as they are need no worker, only to wait their turn
	// behind the network: a couple queued keep it busy. Without this bound
	// a file that doesn't compress filled the whole queue, n MiB held.
	plain := make(chan struct{}, plainAhead)
	// Chunks being compressed, or compressed and waiting for the network,
	// are bounded by slots, which grow only while the network waits on
	// compression. On a link slower than one core compresses (most of
	// them, for data that shrinks to a fifth) one encoder and a couple of
	// chunks are all a transfer holds; on a fast one it grows to n.
	slots := newPackSlots(n)
	var bo compressBackoff
	var lastZ atomic.Int64 // the last compressed size: sizes the next copy's buffer
	var workers sync.WaitGroup
	for range n {
		workers.Go(func() {
			for j := range jobs {
				switch {
				case j.try && j.probe && looksIncompressible(j.c.buf):
					// Answered without an encoder: a photo, video or
					// archive never wakes one.
				case j.try:
					slots.startCompress()
					j.z = enc.compress(j.c.buf, getZ(zFor(len(j.c.buf), lastZ.Load())))
					slots.endCompress()
					if j.z != nil {
						lastZ.Store(int64(len(j.z)))
					}
					j.shrank = j.z != nil && len(j.z)+len(j.z)/10 < len(j.c.buf)
				}
				if j.try {
					// Decided here, not by the writer: a probe's answer must
					// arrive even once the connection has failed.
					bo.result(j.shrank, j.probe)
				}
				close(j.done)
			}
		})
	}
	written := make(chan struct{})
	go func() {
		defer close(written)
		prev, primed := time.Now(), false
		for j := range order {
			// cycle: since the writer last went for a chunk, so the write
			// of the one before and this wait, the share judged.
			top := time.Now()
			cycle := top.Sub(prev)
			prev = top
			var waited time.Duration
			select {
			case <-j.done:
			default:
				start := time.Now()
				<-j.done
				waited = time.Since(start)
			}
			if j.z != nil {
				// The first is always waited for: nothing else was ready.
				if primed {
					slots.compressed(waited, cycle+waited)
				}
				primed = true
			}
			p.writeChunk(w, j)
			if j.try {
				slots.release()
			} else {
				<-plain
			}
		}
	}()
	for c := range in {
		j := &packJob{c: c, done: make(chan struct{})}
		if len(c.buf) > 0 && p.failed() == nil {
			j.try, j.probe = bo.shouldTry()
		}
		if j.try {
			slots.acquire()
		} else {
			plain <- struct{}{}
		}
		order <- j
		jobs <- j
	}
	close(jobs)
	close(order)
	workers.Wait()
	<-written
}

// looksIncompressible reports whether b's bytes are spread so evenly that
// zstd can't take a tenth off: the byte entropy of samples across the chunk
// is near the 8 bits of random data. Compressed formats (JPEG, MP4, zip,
// zstd) read about 7.9 or more; text and code about 5, binaries about 6.
// It only spares an encoder: a chunk it lets through is still compressed
// and kept only if it shrank.
func looksIncompressible(b []byte) bool {
	const sample, samples = 4 << 10, 16
	if len(b) < sample*samples {
		return false
	}
	var hist [256]int
	step := len(b) / samples
	for i := range samples {
		for _, c := range b[i*step : i*step+sample] {
			hist[c]++
		}
	}
	n := float64(sample * samples)
	bits := 0.0
	for _, c := range hist {
		if c > 0 {
			p := float64(c) / n
			bits -= p * math.Log2(p)
		}
	}
	return bits > incompressibleBits
}

// incompressibleBits is the byte entropy above which a chunk is taken not
// to compress. zstd's literal coding alone needs entropy well under 8 bits
// to save a tenth (about 7.2), so 7.5 leaves margin both ways.
const incompressibleBits = 7.5

// Compressed copies go in buffers sized to what the data compresses to,
// not in a 1 MiB block each: text shrinks to a fifth, and every chunk in
// flight held its block. The size is guessed from the chunk before (zFor);
// a copy that outgrows its buffer has it grown by EncodeAll, once, and the
// next guess is larger.
var zClasses = [...]int{128 << 10, 256 << 10, 512 << 10}

var zFree = func() (f [len(zClasses)]chan []byte) {
	for i := range f {
		f[i] = make(chan []byte, keepZ)
	}
	return f
}()

// keepZ bounds each class's free list: the chunks in compression at once.
const keepZ = 16

// zFor is the room to give the compressed copy of an n-byte chunk when
// the one before compressed to last bytes (0: none yet, so a full block,
// which a chunk that doesn't shrink fits: blockSlack).
func zFor(n int, last int64) int {
	if last <= 0 {
		return hashBlockSize + blockSlack
	}
	return min(int(last)+int(last)/4+16<<10, n+n>>8+64)
}

// getZ returns an empty buffer with at least want bytes of room.
func getZ(want int) []byte {
	for i, c := range zClasses {
		if want <= c {
			select {
			case b := <-zFree[i]:
				return b[:0]
			default:
				return make([]byte, 0, c)
			}
		}
	}
	return getBlock()[:0]
}

// putZ recycles a buffer from getZ; one EncodeAll grew is left to the
// collector.
func putZ(b []byte) {
	for i, c := range zClasses {
		if cap(b) == c {
			select {
			case zFree[i] <- b[:0]:
			default:
			}
			return
		}
	}
	putBlock(b)
}

// packSlots bounds compression: how many chunks compress at once (each
// wakes an encoder), and how many are tried in all, compressing or
// compressed and waiting for the network (each holds the chunk and its
// compressed copy). One more chunk is in flight than compress, so the next
// compresses while one is written. The limit starts at one and grows by
// one when, over the last growWindow compressed chunks, the writer spent
// more than a quarter of its time waiting for compression rather than for
// the network: a transfer only pays for the encoders that keep its link
// busy. A brief wait doesn't count: a link that takes data in bursts
// (several QUIC lanes) has the writer wait a moment between them while
// the network is still the limit. It doesn't shrink: what it grew to, it
// already holds.
type packSlots struct {
	mu                    sync.Mutex
	cond                  *sync.Cond
	inFlight, compressing int
	limit, max            int
	seen                  int           // compressed chunks in this window
	waited, spent         time.Duration // the writer's waits for them, and its time
}

// growWindow is how many compressed chunks the writer's waiting is
// judged over before another encoder may join.
const growWindow = 4

func newPackSlots(max int) *packSlots {
	s := &packSlots{limit: 1, max: max}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// acquire waits for room for another tried chunk; release, once written.
func (s *packSlots) acquire() {
	s.mu.Lock()
	for s.inFlight >= s.limit+1 {
		s.cond.Wait()
	}
	s.inFlight++
	s.mu.Unlock()
}

func (s *packSlots) release() {
	s.mu.Lock()
	s.inFlight--
	s.cond.Broadcast()
	s.mu.Unlock()
}

// startCompress waits for an encoder's turn; endCompress gives it back.
func (s *packSlots) startCompress() {
	s.mu.Lock()
	for s.compressing >= s.limit {
		s.cond.Wait()
	}
	s.compressing++
	s.mu.Unlock()
}

func (s *packSlots) endCompress() {
	s.mu.Lock()
	s.compressing--
	s.cond.Broadcast()
	s.mu.Unlock()
}

// compressed reports a compressed chunk reaching the writer: how long the
// writer waited for it, out of the time since it went for the chunk before
// (that one's write, and this wait).
func (s *packSlots) compressed(waited, spent time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen++
	s.waited += waited
	s.spent += spent
	if s.seen < growWindow {
		return
	}
	if s.waited*4 > s.spent && s.limit < s.max {
		s.limit++
		s.cond.Broadcast()
	}
	s.seen, s.waited, s.spent = 0, 0, 0
}

// plainAhead is how many uncompressed chunks may wait for the network.
const plainAhead = 2

// writeChunk writes one finished job, compressed if that shrank it by
// more than a tenth, and returns its buffers to the pool.
func (p *chunkPacker) writeChunk(w io.Writer, j *packJob) {
	defer j.c.free()
	if j.z != nil {
		defer putZ(j.z)
	}
	if p.failed() != nil {
		return
	}
	ch := &wire.Chunk{Segments: j.c.segs, Payload: j.c.buf}
	if j.shrank {
		ch.Compressed, ch.Payload = true, j.z
	}
	if err := wire.WriteChunk(w, ch); err != nil {
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
	}
}

func (p *chunkPacker) failed() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// Close flushes nothing; it stops the writer and reports its first error.
// Call flush first to send buffered bytes.
func (p *chunkPacker) Close() error {
	if p.out != nil {
		close(p.out)
		p.out = nil
		<-p.done
	}
	if p.buf != nil {
		putBlock(p.buf)
		p.buf = nil
	}
	return p.failed()
}

// maxSegmentsPerChunk bounds how many files a single chunk may carry. The wire
// segment count is a uint16, so without this a folder of tens of thousands of
// tiny files would overflow a sub-MiB buffer's segment count and be rejected.
// 4096 stays well under the uint16 limit and keeps the header small.
const maxSegmentsPerChunk = 4096

// needNewSegment reports whether the next bytes for index require opening a
// fresh segment (the buffer is empty, the last segment is a different file, or
// that file already ended).
func (p *chunkPacker) needNewSegment(index uint32) bool {
	n := len(p.segs)
	return n == 0 || p.segs[n-1].FileIndex != index || p.segs[n-1].EOF
}

// markRestart flags the file's next segment: a resumed file is being sent
// from offset 0 because the receiver's partial didn't match.
func (p *chunkPacker) markRestart(index uint32) {
	if p.restart == nil {
		p.restart = make(map[uint32]bool)
	}
	p.restart[index] = true
}

// appendBytes adds data belonging to file index, flushing whenever the byte
// buffer fills or the segment count is exhausted. A flush mid-file closes the
// current (non-EOF) segment; the next append re-opens one for the same index.
func (p *chunkPacker) appendBytes(index uint32, data []byte) error {
	if err := p.failed(); err != nil {
		return err
	}
	for len(data) > 0 {
		space := wire.MaxChunkSize - len(p.buf)
		if space == 0 || (p.needNewSegment(index) && len(p.segs) >= maxSegmentsPerChunk) {
			if err := p.flush(); err != nil {
				return err
			}
			space = wire.MaxChunkSize
		}
		take := min(space, len(data))
		if p.needNewSegment(index) {
			seg := wire.Segment{FileIndex: index}
			if p.restart[index] {
				seg.Restart = true
				delete(p.restart, index)
			}
			p.segs = append(p.segs, seg)
		}
		p.buf = append(p.buf, data[:take]...)
		p.segs[len(p.segs)-1].Length += uint32(take)
		data = data[take:]
		if len(p.buf) == wire.MaxChunkSize {
			if err := p.flush(); err != nil {
				return err
			}
		}
	}
	return nil
}

// adoptBlock sends b, a whole chunk's worth of file index read into a
// pooled block, as a chunk of its own instead of copying it into the
// packer's buffer. It applies only when nothing is buffered (a large file
// starts on a chunk boundary: see alignFor); otherwise it reports false and
// the caller appends the bytes. The caller's hasher reads b too: the block
// returns to the pool once both the hasher (through the returned func) and
// the writer are done with it.
func (p *chunkPacker) adoptBlock(index uint32, b []byte) (hashed func(), ok bool, err error) {
	if err := p.failed(); err != nil {
		return nil, false, err
	}
	if len(b) != wire.MaxChunkSize || len(p.segs) > 0 || len(p.buf) > 0 {
		return nil, false, nil
	}
	seg := wire.Segment{FileIndex: index, Length: wire.MaxChunkSize}
	if p.restart[index] {
		seg.Restart = true
		delete(p.restart, index)
	}
	release := shareBlock(b, 2)
	p.out <- packed{segs: []wire.Segment{seg}, buf: b, release: release}
	return release, true, p.failed()
}

// alignFor starts a new chunk before a file with at least a chunk of bytes
// still to send, so its blocks line up with chunks and go without a copy
// (adoptBlock). Small files keep packing together.
func (p *chunkPacker) alignFor(remaining uint64) error {
	if remaining < wire.MaxChunkSize || len(p.segs) == 0 {
		return p.failed()
	}
	return p.flush()
}

// endFile marks file index complete with its root hash. If the file's bytes
// ended exactly on a chunk boundary, a zero-length EOF segment carries the
// marker. changed reports that the source changed while it was read.
func (p *chunkPacker) endFile(index uint32, root [32]byte, changed bool) error {
	if n := len(p.segs); n > 0 && p.segs[n-1].FileIndex == index && !p.segs[n-1].EOF {
		p.segs[n-1].EOF = true
		p.segs[n-1].RootHash = root
		p.segs[n-1].Changed = changed
		return nil
	}
	return p.addMarker(wire.Segment{FileIndex: index, EOF: true, RootHash: root, Changed: changed})
}

// skipFile tells the receiver the file is abandoned (unreadable at the
// source), after any of its bytes already buffered.
func (p *chunkPacker) skipFile(index uint32) error {
	delete(p.restart, index)
	return p.addMarker(wire.Segment{FileIndex: index, Skip: true})
}

func (p *chunkPacker) addMarker(seg wire.Segment) error {
	if err := p.failed(); err != nil {
		return err
	}
	// The marker needs its own segment; flush first if the chunk is full.
	if len(p.segs) >= maxSegmentsPerChunk {
		if err := p.flush(); err != nil {
			return err
		}
	}
	p.segs = append(p.segs, seg)
	return nil
}

// flush hands the buffered segments to the writer as one chunk.
func (p *chunkPacker) flush() error {
	if len(p.segs) == 0 {
		return p.failed()
	}
	p.out <- packed{segs: p.segs, buf: p.buf}
	p.buf = getBlock()[:0]
	p.segs = nil
	return p.failed()
}

// decodeChunkPayload returns the uncompressed payload of c, decompressing
// into dst when given. dec is created lazily and reused; the caller owns
// Close. Content is verified per file by its tree root, not per chunk.
func decodeChunkPayload(c *wire.Chunk, dec **zstd.Decoder, dst []byte) ([]byte, error) {
	plain := c.Payload
	if c.Compressed {
		if *dec == nil {
			d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(2*wire.MaxChunkSize), zstd.WithDecoderConcurrency(1))
			if err != nil {
				return nil, fmt.Errorf("recv: zstd reader: %w", err)
			}
			*dec = d
		}
		var err error
		plain, err = (*dec).DecodeAll(c.Payload, dst[:0])
		if err != nil {
			return nil, fmt.Errorf("%w: zstd decode: %v", fserrors.ErrProtocolError, err)
		}
	}
	if len(plain) > wire.MaxChunkSize {
		return nil, fmt.Errorf("%w: decompressed chunk %d > limit %d", fserrors.ErrProtocolError, len(plain), wire.MaxChunkSize)
	}
	if uint64(len(plain)) != c.SegmentsSum() {
		return nil, fmt.Errorf("%w: payload %d != segment sum %d", fserrors.ErrProtocolError, len(plain), c.SegmentsSum())
	}
	return plain, nil
}
