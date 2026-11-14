package transfer

import (
	"errors"
	"fmt"
	"io"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// maxListingBatchBytes keeps a batch comfortably under MaxControlFrameSize.
const maxListingBatchBytes = 56 * 1024

// maxListingEntries caps a single transfer's entry count, bounding receiver
// memory against a hostile sender streaming an unbounded listing: the
// receiver holds each entry, its plan and a name key, roughly 0.5 KB per
// entry, so 4M entries stay around 2 GB at worst. That covers monorepos with
// node_modules, maildirs and photo libraries; the sender checks the count
// before pairing and the receiver reports E051, not a version error.
const maxListingEntries = 4_000_000

// MaxEntries is maxListingEntries for the CLI's pre-flight check.
const MaxEntries = maxListingEntries

// zListingBatchBytes is the batch estimate when the listing is compressed:
// eight plain batches' worth, which still compresses well under a frame.
const zListingBatchBytes = 8 * maxListingBatchBytes

// sendListing streams the sources' entries as batched control frames followed
// by TypeListingEnd — zstd-compressed when the receiver advertised
// CapCompressedListing.
func sendListing(w io.Writer, sources []Source, compressed bool) error {
	limit := maxListingBatchBytes
	if compressed {
		limit = zListingBatchBytes
	}
	var batch []wire.ListingEntry
	var size int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		var err error
		if compressed {
			err = writeZBatch(w, batch)
		} else {
			err = wire.WriteControl(w, wire.TypeListingBatch, batch)
		}
		batch, size = nil, 0
		return err
	}
	for _, s := range sources {
		est := len(s.Entry.RelativePath) + 48
		if len(batch) > 0 && size+est > limit {
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

// writeZBatch writes a compressed batch, halving it until each part fits
// a control frame (names that compress poorly).
func writeZBatch(w io.Writer, batch []wire.ListingEntry) error {
	err := wire.WriteListingBatchZ(w, batch)
	if errors.Is(err, wire.ErrFrameTooLarge) && len(batch) > 1 {
		half := len(batch) / 2
		if err := writeZBatch(w, batch[:half]); err != nil {
			return err
		}
		return writeZBatch(w, batch[half:])
	}
	return err
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
		case wire.TypeListingBatch, wire.TypeListingBatchZ:
			var batch []wire.ListingEntry
			var err error
			if ft == wire.TypeListingBatchZ {
				batch, err = wire.DecodeListingBatchZ(body)
			} else {
				err = wire.Decode(body, &batch)
			}
			if err != nil {
				return nil, fmt.Errorf("%w: listing decode: %v", fserrors.ErrProtocolError, err)
			}
			entries = append(entries, batch...)
			if len(entries) > maxListingEntries {
				return nil, fmt.Errorf("%w: more than %d entries", fserrors.ErrTooManyEntries, maxListingEntries)
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

// validateListing enforces contiguous 0..n-1 indices. Duplicate and colliding
// names are judged later, against the receiving filesystem's own rules
// (classify), so a case-only pair is fine between case-sensitive systems.
func validateListing(entries []wire.ListingEntry) error {
	for i, e := range entries {
		if e.Index != uint32(i) {
			return fmt.Errorf("%w: listing index %d out of order at position %d", fserrors.ErrProtocolError, e.Index, i)
		}
	}
	return nil
}

// sendDecisions streams decisions as batched control frames then end. A
// resume decision carries a 32-byte root, so batches are sized by estimate.
func sendDecisions(w io.Writer, decisions []wire.Decision) error {
	const budget = 48 * 1024
	start, size := 0, 0
	for i := range decisions {
		est := 24 + len(decisions[i].PrefixRoot)*2
		if i > start && size+est > budget {
			if err := wire.WriteControl(w, wire.TypeClassifyBatch, decisions[start:i]); err != nil {
				return err
			}
			start, size = i, 0
		}
		size += est
	}
	if start < len(decisions) {
		if err := wire.WriteControl(w, wire.TypeClassifyBatch, decisions[start:]); err != nil {
			return err
		}
	}
	return wire.WriteControl(w, wire.TypeClassifyEnd, nil)
}
