package transfer

import (
	"fmt"
	"io"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// maxListingBatchBytes keeps a batch comfortably under MaxControlFrameSize.
const maxListingBatchBytes = 56 * 1024

// maxListingEntries caps a single transfer's entry count, bounding receiver
// memory against a hostile sender streaming an unbounded listing.
const maxListingEntries = 5_000_000

// sendListing streams the sources' entries as batched control frames followed
// by TypeListingEnd.
func sendListing(w io.Writer, sources []Source) error {
	var batch []wire.ListingEntry
	var size int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := wire.WriteControl(w, wire.TypeListingBatch, batch); err != nil {
			return err
		}
		batch, size = nil, 0
		return nil
	}
	for _, s := range sources {
		est := len(s.Entry.RelativePath) + len(s.Entry.SymlinkTarget) + 48
		if len(batch) > 0 && size+est > maxListingBatchBytes {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, s.Entry)
		size += est
	}
	if err := flush(); err != nil {
		return err
	}
	return wire.WriteControl(w, wire.TypeListingEnd, nil)
}

// recvListing reads listing batches until TypeListingEnd, validating the
// entries form a contiguous, collision-free set.
func recvListing(r io.Reader) ([]wire.ListingEntry, error) {
	var entries []wire.ListingEntry
	for {
		ft, body, err := wire.ReadControlRaw(r)
		if err != nil {
			return nil, fmt.Errorf("recv: listing: %w", err)
		}
		switch ft {
		case wire.TypeListingBatch:
			var batch []wire.ListingEntry
			if err := wire.Decode(body, &batch); err != nil {
				return nil, fmt.Errorf("recv: listing decode: %w", err)
			}
			entries = append(entries, batch...)
			if len(entries) > maxListingEntries {
				return nil, fmt.Errorf("%w: listing exceeds %d entries", fserrors.ErrProtocolError, maxListingEntries)
			}
		case wire.TypeListingEnd:
			if err := validateListing(entries); err != nil {
				return nil, err
			}
			return entries, nil
		case wire.TypeError:
			var ef wire.ErrorFrame
			_ = wire.Decode(body, &ef)
			return nil, mapPeerError(ef)
		default:
			return nil, fmt.Errorf("%w: expected listing, got %v", fserrors.ErrProtocolError, ft)
		}
	}
}

// validateListing enforces contiguous 0..n-1 indices and rejects duplicate or
// case-colliding paths — the listing is the receiver's consent, so it must be
// self-consistent before anything lands.
func validateListing(entries []wire.ListingEntry) error {
	seen := make(map[string]bool, len(entries))
	for i, e := range entries {
		if e.Index != uint32(i) {
			return fmt.Errorf("%w: listing index %d out of order at position %d", fserrors.ErrProtocolError, e.Index, i)
		}
		key := lowerASCII(e.RelativePath)
		if seen[key] {
			return fmt.Errorf("%w: duplicate path %q", fserrors.ErrProtocolError, e.RelativePath)
		}
		seen[key] = true
	}
	return nil
}

// sendDecisions streams decisions as batched control frames then end.
func sendDecisions(w io.Writer, decisions []wire.Decision) error {
	const per = 1024
	for i := 0; i < len(decisions); i += per {
		end := i + per
		if end > len(decisions) {
			end = len(decisions)
		}
		if err := wire.WriteControl(w, wire.TypeClassifyBatch, decisions[i:end]); err != nil {
			return err
		}
	}
	return wire.WriteControl(w, wire.TypeClassifyEnd, nil)
}

// recvDecisions reads decision batches until end, returning index→Decision.
func recvDecisions(r io.Reader) (map[uint32]wire.Decision, error) {
	out := make(map[uint32]wire.Decision)
	for {
		ft, body, err := wire.ReadControlRaw(r)
		if err != nil {
			return nil, fmt.Errorf("send: decisions: %w", err)
		}
		switch ft {
		case wire.TypeClassifyBatch:
			var batch []wire.Decision
			if err := wire.Decode(body, &batch); err != nil {
				return nil, fmt.Errorf("send: decisions decode: %w", err)
			}
			for _, d := range batch {
				out[d.Index] = d
			}
		case wire.TypeClassifyEnd:
			return out, nil
		case wire.TypeError:
			var ef wire.ErrorFrame
			_ = wire.Decode(body, &ef)
			return nil, mapPeerError(ef)
		default:
			return nil, fmt.Errorf("%w: expected classification, got %v", fserrors.ErrProtocolError, ft)
		}
	}
}

// lowerASCII lowercases a/z without locale surprises (collision check only).
func lowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}
