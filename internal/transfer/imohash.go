package transfer

import (
	"fmt"
	"io"
	"os"

	"github.com/kalafut/imohash"
)

// ImohashSize is the imohash digest width in bytes (128 bits).
//
// Kept as a local constant so callers don't have to import kalafut/imohash
// just to size a struct field. Must match imohash.Size.
const ImohashSize = imohash.Size

// PrefixImohash returns the imohash digest of the first prefixLen bytes
// of a file, computed as if those bytes were the whole file. Used on the
// sender to validate the receiver's claim that "I have a partial that
// starts at byte 0 and ends at prefixLen."
//
// A fresh imohash.Imohash is used per call rather than a shared instance:
// its SumSectionReader mutates an internal murmur3 state, so a shared
// value would race if PrefixImohash were ever called concurrently. The
// constructor is cheap. The library defaults (16 KiB samples, 128 KiB
// threshold) suit the resume use case — below the threshold a file is
// hashed in full; above it only head/middle/tail are sampled, which gives
// the ~constant-time behavior on huge files. An accidental collision on a
// large file is ~2^-64, low enough for non-adversarial resumes.
//
// We avoid copying the prefix into memory by handing imohash an
// io.SectionReader over the open file.
func PrefixImohash(path string, prefixLen int64) ([ImohashSize]byte, error) {
	var zero [ImohashSize]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, fmt.Errorf("imohash open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	sr := io.NewSectionReader(f, 0, prefixLen)
	hasher := imohash.New()
	h, err := hasher.SumSectionReader(sr)
	if err != nil {
		return zero, fmt.Errorf("imohash prefix %s: %w", path, err)
	}
	return h, nil
}
