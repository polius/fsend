package wire

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"io"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// A listing is mostly repeated path prefixes, so zstd shrinks it about
// fifteen times: a 500,000-entry tree is ~48 MB as plain gob before the
// receiver can ask its first question, and is re-sent on every retry.
// Compressed batches (TypeListingBatchZ) travel only to a receiver that
// advertised CapCompressedListing.

// maxListingInflated bounds one compressed batch once decompressed, so a
// hostile sender can't use a small frame as a decompression bomb.
const maxListingInflated = 2 << 20

var (
	zEncOnce sync.Once
	zEnc     *zstd.Encoder
)

func listingEncoder() *zstd.Encoder {
	zEncOnce.Do(func() {
		// A batch is at most maxListingInflated, so that is all the window
		// it can use: the default 8 MiB window kept a 16 MiB history buffer
		// for the whole process, for nothing.
		zEnc, _ = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault), zstd.WithEncoderConcurrency(1),
			zstd.WithWindowSize(maxListingInflated))
	})
	return zEnc
}

// WriteListingBatchZ writes batch as one zstd-compressed listing frame. It
// returns ErrFrameTooLarge (writing nothing) when the compressed batch would
// not fit a control frame; the caller splits the batch and tries again.
func WriteListingBatchZ(w io.Writer, batch []ListingEntry) error {
	var raw bytes.Buffer
	if err := gob.NewEncoder(&raw).Encode(batch); err != nil {
		return fmt.Errorf("wire: encoding listing: %w", err)
	}
	if raw.Len() > maxListingInflated {
		return fmt.Errorf("%w: listing batch %d bytes uncompressed", ErrFrameTooLarge, raw.Len())
	}
	body := listingEncoder().EncodeAll(raw.Bytes(), nil)
	if len(body) > MaxControlFrameSize {
		return fmt.Errorf("%w: compressed listing batch %d bytes", ErrFrameTooLarge, len(body))
	}
	header := [controlHeaderSize]byte{Stamp(w, TypeListingBatchZ), byte(TypeListingBatchZ)}
	binary.BigEndian.PutUint32(header[2:6], uint32(len(body))) //nolint:gosec // ≤ MaxControlFrameSize, checked above
	if _, err := w.Write(header[:]); err != nil {
		return fmt.Errorf("wire: writing header: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("wire: writing body: %w", err)
	}
	return nil
}

// inflatePool recycles the buffers listing batches inflate into: a folder
// of many files arrives in many batches, and each took 2 MiB afresh.
var inflatePool = sync.Pool{New: func() any {
	b := make([]byte, 0, maxListingInflated)
	return &b
}}

// DecodeListingBatchZ inflates and decodes a TypeListingBatchZ body,
// refusing anything that inflates past maxListingInflated.
func DecodeListingBatchZ(body []byte) ([]ListingEntry, error) {
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1),
		zstd.WithDecoderMaxMemory(maxListingInflated), zstd.WithDecodeAllCapLimit(true))
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	bp := inflatePool.Get().(*[]byte)
	defer inflatePool.Put(bp)
	raw, err := dec.DecodeAll(body, (*bp)[:0])
	if err != nil {
		return nil, fmt.Errorf("wire: inflating listing: %w", err)
	}
	var batch []ListingEntry
	if err := gob.NewDecoder(bytes.NewReader(raw)).Decode(&batch); err != nil {
		return nil, fmt.Errorf("wire: decoding listing: %w", err)
	}
	return batch, nil
}
