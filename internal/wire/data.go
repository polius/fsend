package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Data-stream chunk frame layout:
//
//   type(1)=0x10 | flags(1) | payload_len(u32 BE) | segment_count(u16 BE) |
//   per segment: file_index(u32) | length(u32) | seg_flags(u8)
//                | [root_hash(32) iff seg_flags&SegFlagEOF] |
//   payload(payload_len bytes, zstd iff flags&FlagCompressed)
//
// A chunk carries either a slice of one large file (one segment) or several
// whole small files (many segments), so tiny files still compress together.
// There is no per-chunk hash: QUIC's AEAD already authenticates every byte in
// transit, and each segment's root_hash (on its EOF) verifies the whole file
// end to end.
//
// The version byte is dropped here vs. control frames — the data stream is
// only opened after the control HELLO/ACK negotiated a version.

const chunkFixedHeader = 1 + 1 + 4 + 2 // type+flags+payload_len+segment_count

// ErrChunkTooLarge is returned when a chunk's declared payload exceeds MaxChunkSize.
var ErrChunkTooLarge = errors.New("wire: chunk payload too large")

// ErrWrongFrameType is returned when a data-stream read encounters a non-chunk type.
var ErrWrongFrameType = errors.New("wire: wrong frame type on data stream")

// ErrMalformedChunk is returned when a chunk's segment metadata is inconsistent.
var ErrMalformedChunk = errors.New("wire: malformed chunk")

// Segment describes one file's bytes within a chunk's payload, in order.
type Segment struct {
	FileIndex uint32
	Length    uint32   // bytes of this segment in the uncompressed payload
	EOF       bool     // last segment of this file → RootHash valid
	Skip      bool     // file abandoned by the sender (Length 0, no EOF)
	Restart   bool     // resumed file restarts at offset 0 with these bytes
	Changed   bool     // with EOF: the source changed while it was read
	RootHash  [32]byte // tree root of the whole file; only when EOF
}

func (s *Segment) flags() uint8 {
	var f uint8
	if s.EOF {
		f |= SegFlagEOF
	}
	if s.Skip {
		f |= SegFlagSkip
	}
	if s.Restart {
		f |= SegFlagRestart
	}
	if s.Changed {
		f |= SegFlagChanged
	}
	return f
}

// Chunk is the in-memory representation of one data-stream frame.
// Payload holds the on-wire bytes (zstd-compressed iff Compressed).
type Chunk struct {
	Compressed bool
	Segments   []Segment
	Payload    []byte
}

// WriteChunk serializes c and writes it to w. c.Payload must be ≤ MaxChunkSize.
func WriteChunk(w io.Writer, c *Chunk) error {
	if len(c.Payload) > MaxChunkSize {
		return fmt.Errorf("%w: %d > %d", ErrChunkTooLarge, len(c.Payload), MaxChunkSize)
	}
	if len(c.Segments) == 0 || len(c.Segments) > 0xFFFF {
		return fmt.Errorf("%w: %d segments", ErrMalformedChunk, len(c.Segments))
	}

	var flags uint8
	if c.Compressed {
		flags = FlagCompressed
	}

	header := make([]byte, chunkFixedHeader, chunkFixedHeader+len(c.Segments)*(9+32)+len(c.Payload))
	header[0] = byte(TypeChunk)
	header[1] = flags
	binary.BigEndian.PutUint32(header[2:6], uint32(len(c.Payload)))
	binary.BigEndian.PutUint16(header[6:8], uint16(len(c.Segments)))

	for i := range c.Segments {
		s := &c.Segments[i]
		var seg [9]byte
		binary.BigEndian.PutUint32(seg[0:4], s.FileIndex)
		binary.BigEndian.PutUint32(seg[4:8], s.Length)
		seg[8] = s.flags()
		header = append(header, seg[:]...)
		if s.EOF {
			header = append(header, s.RootHash[:]...)
		}
	}

	// One write per frame: the header and payload travel together, so a
	// stream-backed writer doesn't pay a syscall (or a QUIC frame) per half.
	if _, err := w.Write(append(header, c.Payload...)); err != nil {
		return fmt.Errorf("wire: writing chunk: %w", err)
	}
	return nil
}

// ReadChunk reads one chunk frame from r. It validates the frame's internal
// consistency (segment count, and — when uncompressed — that the segment
// lengths sum to the payload size); the caller verifies the hashes.
func ReadChunk(r io.Reader) (*Chunk, error) { return ReadChunkBuf(r, nil) }

// ReadChunkBuf is ReadChunk reading the payload into buf when it fits, so a
// receiver can recycle payload buffers.
func ReadChunkBuf(r io.Reader, buf []byte) (*Chunk, error) {
	var fixed [chunkFixedHeader]byte
	if _, err := io.ReadFull(r, fixed[:]); err != nil {
		return nil, err // surface EOF unwrapped
	}
	if FrameType(fixed[0]) != TypeChunk {
		return nil, fmt.Errorf("%w: expected 0x%02X, got 0x%02X", ErrWrongFrameType, TypeChunk, fixed[0])
	}

	c := &Chunk{Compressed: fixed[1]&FlagCompressed != 0}
	payloadLen := binary.BigEndian.Uint32(fixed[2:6])
	if payloadLen > MaxChunkSize {
		return nil, fmt.Errorf("%w: declared %d > limit %d", ErrChunkTooLarge, payloadLen, MaxChunkSize)
	}
	segCount := binary.BigEndian.Uint16(fixed[6:8])
	if segCount == 0 {
		return nil, fmt.Errorf("%w: zero segments", ErrMalformedChunk)
	}

	// Grow the segment slice as header bytes arrive instead of pre-allocating
	// all segCount entries: a hostile 40-byte header must not buy megabytes of
	// allocation for free. Honest chunks carry at most a few thousand segments.
	c.Segments = make([]Segment, 0, min(int(segCount), 64))
	var sum uint64
	for i := 0; i < int(segCount); i++ {
		var seg [9]byte
		if _, err := io.ReadFull(r, seg[:]); err != nil {
			return nil, fmt.Errorf("wire: reading segment header: %w", err)
		}
		s := Segment{
			FileIndex: binary.BigEndian.Uint32(seg[0:4]),
			Length:    binary.BigEndian.Uint32(seg[4:8]),
			EOF:       seg[8]&SegFlagEOF != 0,
			Skip:      seg[8]&SegFlagSkip != 0,
			Restart:   seg[8]&SegFlagRestart != 0,
			Changed:   seg[8]&SegFlagChanged != 0,
		}
		if err := s.validate(); err != nil {
			return nil, err
		}
		if s.EOF {
			if _, err := io.ReadFull(r, s.RootHash[:]); err != nil {
				return nil, fmt.Errorf("wire: reading segment root hash: %w", err)
			}
		}
		sum += uint64(s.Length)
		c.Segments = append(c.Segments, s)
	}
	if sum > MaxChunkSize {
		return nil, fmt.Errorf("%w: segment lengths sum %d > limit %d", ErrChunkTooLarge, sum, MaxChunkSize)
	}
	// When uncompressed the payload is the concatenation, so the lengths must
	// sum to it exactly. Compressed payloads are checked after decode.
	if !c.Compressed && sum != uint64(payloadLen) {
		return nil, fmt.Errorf("%w: segment lengths %d != payload %d", ErrMalformedChunk, sum, payloadLen)
	}

	if payloadLen > 0 {
		if uint32(cap(buf)) >= payloadLen {
			c.Payload = buf[:payloadLen]
		} else {
			c.Payload = make([]byte, payloadLen)
		}
		if _, err := io.ReadFull(r, c.Payload); err != nil {
			return nil, fmt.Errorf("wire: reading chunk payload: %w", err)
		}
	}
	return c, nil
}

// validate rejects flag combinations no honest sender emits. Empty data
// segments are refused so a peer can't open files (or burn CPU) for free.
func (s *Segment) validate() error {
	switch {
	case s.Skip && (s.EOF || s.Restart || s.Changed || s.Length != 0):
		return fmt.Errorf("%w: skip segment with data or flags", ErrMalformedChunk)
	case s.Changed && !s.EOF:
		return fmt.Errorf("%w: changed flag without EOF", ErrMalformedChunk)
	case s.Restart && s.Length == 0:
		return fmt.Errorf("%w: empty restart segment", ErrMalformedChunk)
	case s.Length == 0 && !s.EOF && !s.Skip:
		return fmt.Errorf("%w: empty segment", ErrMalformedChunk)
	}
	return nil
}

// SegmentsSum returns the total uncompressed byte length of c's segments.
func (c *Chunk) SegmentsSum() uint64 {
	var sum uint64
	for _, s := range c.Segments {
		sum += uint64(s.Length)
	}
	return sum
}
