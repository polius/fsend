package transfer

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// TestRecvListing_CapEnforced pins the B9 memory bound: a hostile sender
// streaming a listing past maxListingEntries is cut off with ErrProtocolError
// instead of the receiver materialising an unbounded entry set.
func TestRecvListing_CapEnforced(t *testing.T) {
	var buf bytes.Buffer
	const batchSize = 1000 // stays under MaxControlFrameSize
	total := 0
	for total <= maxListingEntries {
		batch := make([]wire.ListingEntry, batchSize)
		for i := range batch {
			batch[i] = wire.ListingEntry{
				Type:         wire.EntryFile,
				Index:        uint32(total + i),
				RelativePath: fmt.Sprintf("f%07d", total+i),
				Size:         1,
			}
		}
		if err := wire.WriteControl(&buf, wire.TypeListingBatch, batch); err != nil {
			t.Fatalf("write batch at %d: %v", total, err)
		}
		total += batchSize
	}
	if err := wire.WriteControl(&buf, wire.TypeListingEnd, nil); err != nil {
		t.Fatalf("write end: %v", err)
	}

	if _, err := recvListing(&buf); !errors.Is(err, fserrors.ErrProtocolError) {
		t.Fatalf("recvListing err = %v, want ErrProtocolError", err)
	}
}
