package transfer

import (
	"fmt"
	"io"
	"os"

	"github.com/klauspost/compress/zstd"
	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// chunkPacker streams file bytes onto the data stream, packing several whole
// small files into one chunk (so they compress together) while slicing large
// files across chunks. It maintains the per-chunk hash and emits each file's
// BLAKE3 root on its EOF segment.
type chunkPacker struct {
	w    io.Writer
	enc  *zstd.Encoder
	buf  []byte
	segs []wire.Segment
}

func newChunkPacker(w io.Writer) (*chunkPacker, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("send: zstd writer: %w", err)
	}
	return &chunkPacker{w: w, enc: enc, buf: make([]byte, 0, wire.MaxChunkSize)}, nil
}

func (p *chunkPacker) Close() { _ = p.enc.Close() }

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

// appendBytes adds data belonging to file index, flushing whenever the byte
// buffer fills or the segment count is exhausted. A flush mid-file closes the
// current (non-EOF) segment; the next append re-opens one for the same index.
func (p *chunkPacker) appendBytes(index uint32, data []byte) error {
	for len(data) > 0 {
		space := wire.MaxChunkSize - len(p.buf)
		if space == 0 || (p.needNewSegment(index) && len(p.segs) >= maxSegmentsPerChunk) {
			if err := p.flush(); err != nil {
				return err
			}
			space = wire.MaxChunkSize
		}
		take := space
		if len(data) < take {
			take = len(data)
		}
		if p.needNewSegment(index) {
			p.segs = append(p.segs, wire.Segment{FileIndex: index})
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
// marker.
func (p *chunkPacker) endFile(index uint32, root [32]byte) error {
	if n := len(p.segs); n > 0 && p.segs[n-1].FileIndex == index && !p.segs[n-1].EOF {
		p.segs[n-1].EOF = true
		p.segs[n-1].RootHash = root
		return nil
	}
	// The marker needs its own segment; flush first if the chunk is full.
	if len(p.segs) >= maxSegmentsPerChunk {
		if err := p.flush(); err != nil {
			return err
		}
	}
	p.segs = append(p.segs, wire.Segment{FileIndex: index, EOF: true, RootHash: root})
	return nil
}

// flush writes the buffered segments as one chunk (compressed if it helps) and
// resets the buffer.
func (p *chunkPacker) flush() error {
	if len(p.segs) == 0 {
		return nil
	}
	c := &wire.Chunk{ChunkHash: blakeHash32(p.buf), Segments: p.segs, Payload: p.buf}
	if len(p.buf) > 0 {
		compressed := p.enc.EncodeAll(p.buf, nil)
		if len(compressed)+len(compressed)/10 < len(p.buf) {
			c.Compressed = true
			c.Payload = compressed
		}
	}
	if err := wire.WriteChunk(p.w, c); err != nil {
		return err
	}
	p.buf = make([]byte, 0, wire.MaxChunkSize)
	p.segs = nil
	return nil
}

// decodeChunkPayload returns the uncompressed payload of c, verifying the
// per-chunk hash. dec is created lazily and reused; the caller owns Close.
func decodeChunkPayload(c *wire.Chunk, dec **zstd.Decoder) ([]byte, error) {
	plain := c.Payload
	if c.Compressed {
		if *dec == nil {
			d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(2*wire.MaxChunkSize))
			if err != nil {
				return nil, fmt.Errorf("recv: zstd reader: %w", err)
			}
			*dec = d
		}
		var err error
		plain, err = (*dec).DecodeAll(c.Payload, nil)
		if err != nil {
			return nil, fmt.Errorf("recv: zstd decode: %w", err)
		}
	}
	if len(plain) > wire.MaxChunkSize {
		return nil, fmt.Errorf("%w: decompressed chunk %d > limit %d", fserrors.ErrProtocolError, len(plain), wire.MaxChunkSize)
	}
	if uint64(len(plain)) != c.SegmentsSum() {
		return nil, fmt.Errorf("%w: payload %d != segment sum %d", fserrors.ErrProtocolError, len(plain), c.SegmentsSum())
	}
	if blakeHash32(plain) != c.ChunkHash {
		return nil, fserrors.ErrHashMismatch
	}
	return plain, nil
}

// blakeHash32 is the BLAKE3 of b as a fixed array.
func blakeHash32(b []byte) [32]byte {
	h := blake3.New()
	_, _ = h.Write(b)
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// hashFileRoot streams a file through BLAKE3 and returns its 32-byte root.
// Used for --checksum content verification on both peers.
func hashFileRoot(path string) ([32]byte, error) {
	var zero [32]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer func() { _ = f.Close() }()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return zero, err
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}
