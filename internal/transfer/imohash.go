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

// imohashHasher is a single shared hasher with the library's default
// parameters (16 KiB samples, 128 KiB threshold). Below the threshold a
// file is hashed in full; above it, only the head, middle, and tail are
// sampled. This is what gives the ~constant-time behavior on huge files.
//
// The default is appropriate for fsend's resume use case: collisions on
// small files cost a re-transfer of a small file (cheap); on large files
// the chance of an accidental collision is ~2^-64 (low enough not to
// worry about for non-adversarial resumes).
var imohashHasher = imohash.New()

// FileImohash returns the imohash digest of a file on disk.
//
// Cost is constant for files above SampleThreshold (~128 KiB): three
// 16 KiB samples + the file size. Use this on the receiver to fingerprint
// a partial file before sending the resume offer.
func FileImohash(path string) ([ImohashSize]byte, error) {
	var zero [ImohashSize]byte
	h, err := imohashHasher.SumFile(path)
	if err != nil {
		return zero, fmt.Errorf("imohash %s: %w", path, err)
	}
	return h, nil
}

// PrefixImohash returns the imohash digest of the first prefixLen bytes
// of a file, computed as if those bytes were the whole file. Used on the
// sender to validate the receiver's claim that "I have a partial that
// starts at byte 0 and ends at prefixLen."
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
	h, err := imohashHasher.SumSectionReader(sr)
	if err != nil {
		return zero, fmt.Errorf("imohash prefix %s: %w", path, err)
	}
	return h, nil
}
