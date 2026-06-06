package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Data-stream chunk frame layout:
//
//   +--------+--------+--------+--------+--------+--------+--------+--------+
//   |  type(1) = 0x10  |  flags(1)     |     chunk_length(uint32 BE)         |
//   +--------+--------+--------+--------+--------+--------+--------+--------+
//   |     file_index(uint32 BE)       |     chunk_index(uint32 BE)          |
//   +--------+--------+--------+--------+--------+--------+--------+--------+
//   |                       blake3_chunk_hash (32 bytes)                    |
//   +-----------------------------------------------------------------------+
//   |                       payload (chunk_length bytes)                    |
//   +-----------------------------------------------------------------------+
//
// The version byte is dropped here vs. control frames — the data stream is
// only opened after the control HELLO/ACK negotiation has agreed on a
// version. Saves ~10% header overhead on small chunks.

const chunkHeaderSize = 1 + 1 + 4 + 4 + 4 + 32 // type+flags+len+file_idx+chunk_idx+hash = 46

// Chunk is the in-memory representation of one data-stream frame.
type Chunk struct {
	Flags      uint8    // FlagCompressed, FlagLastChunk
	FileIndex  uint32   // matches FileInfo.Index
	ChunkIndex uint32   // 0-based within the file
	Blake3Hash [32]byte // BLAKE3 of the *uncompressed* payload
	Payload    []byte   // length ≤ MaxChunkSize (may be compressed if FlagCompressed)
}

// ErrChunkTooLarge is returned when a chunk's declared payload exceeds MaxChunkSize.
var ErrChunkTooLarge = errors.New("wire: chunk payload too large")

// ErrWrongFrameType is returned when a data-stream read encounters a non-chunk type.
var ErrWrongFrameType = errors.New("wire: wrong frame type on data stream")

// WriteChunk serializes c and writes it to w.
//
// c.Payload may be up to MaxChunkSize bytes. The caller is responsible for
// computing c.Blake3Hash over the *uncompressed* payload before calling.
func WriteChunk(w io.Writer, c *Chunk) error {
	if len(c.Payload) > MaxChunkSize {
		return fmt.Errorf("%w: %d > %d", ErrChunkTooLarge, len(c.Payload), MaxChunkSize)
	}

	var header [chunkHeaderSize]byte
	header[0] = byte(TypeChunk)
	header[1] = c.Flags
	binary.BigEndian.PutUint32(header[2:6], uint32(len(c.Payload)))
	binary.BigEndian.PutUint32(header[6:10], c.FileIndex)
	binary.BigEndian.PutUint32(header[10:14], c.ChunkIndex)
	copy(header[14:46], c.Blake3Hash[:])

	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("wire: writing chunk header: %w", err)
	}
	if len(c.Payload) > 0 {
		if _, err := w.Write(c.Payload); err != nil {
			return fmt.Errorf("wire: writing chunk payload: %w", err)
		}
	}
	return nil
}

// ReadChunk reads one chunk frame from r.
//
// Allocates a fresh payload slice each call. Callers transferring at high
// throughput should consider reusing chunks via a sync.Pool at a higher
// level; this function intentionally keeps things simple.
func ReadChunk(r io.Reader) (*Chunk, error) {
	var header [chunkHeaderSize]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err // surface EOF unwrapped
	}
	ft := FrameType(header[0])
	if ft != TypeChunk {
		return nil, fmt.Errorf("%w: expected 0x%02X, got 0x%02X", ErrWrongFrameType, TypeChunk, ft)
	}

	c := &Chunk{
		Flags:      header[1],
		FileIndex:  binary.BigEndian.Uint32(header[6:10]),
		ChunkIndex: binary.BigEndian.Uint32(header[10:14]),
	}
	copy(c.Blake3Hash[:], header[14:46])

	length := binary.BigEndian.Uint32(header[2:6])
	if length > MaxChunkSize {
		return nil, fmt.Errorf("%w: declared %d > limit %d", ErrChunkTooLarge, length, MaxChunkSize)
	}
	if length > 0 {
		c.Payload = make([]byte, length)
		if _, err := io.ReadFull(r, c.Payload); err != nil {
			return nil, fmt.Errorf("wire: reading chunk payload: %w", err)
		}
	}
	return c, nil
}
