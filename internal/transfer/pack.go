package transfer

import (
	"fmt"
	"io"
	"sync"

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
	segs []wire.Segment
	buf  []byte
}

func newChunkPacker(w io.Writer) (*chunkPacker, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("send: zstd writer: %w", err)
	}
	p := &chunkPacker{buf: getBlock()[:0], out: make(chan packed, 2), done: make(chan struct{})}
	go p.writeLoop(p.out, w, enc)
	return p, nil
}

// writeLoop compresses (when it pays) and writes chunks in order. After a
// chunk that doesn't shrink it stops trying for a growing number of chunks,
// so incompressible data doesn't burn a core on zstd.
func (p *chunkPacker) writeLoop(in <-chan packed, w io.Writer, enc *zstd.Encoder) {
	defer close(p.done)
	defer func() { _ = enc.Close() }()
	skip, backoff := 0, 0
	for c := range in {
		if p.failed() != nil {
			putBlock(c.buf)
			continue
		}
		ch := &wire.Chunk{Segments: c.segs, Payload: c.buf}
		var zbuf []byte
		if len(c.buf) > 0 {
			if skip > 0 {
				skip--
			} else {
				zbuf = enc.EncodeAll(c.buf, getBlock()[:0])
				if len(zbuf)+len(zbuf)/10 < len(c.buf) {
					ch.Compressed, ch.Payload = true, zbuf
					backoff = 0
				} else {
					backoff = min(max(1, backoff*2), 64)
					skip = backoff
				}
			}
		}
		err := wire.WriteChunk(w, ch)
		putBlock(c.buf)
		if zbuf != nil {
			putBlock(zbuf)
		}
		if err != nil {
			p.mu.Lock()
			p.err = err
			p.mu.Unlock()
		}
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
