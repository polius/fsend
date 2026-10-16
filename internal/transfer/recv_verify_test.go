package transfer

import (
	"bytes"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/wire"
)

// TestVerifyResponse_FitsFrame pins the invariant behind maxVerifyIDsPerFrame:
// a full batch's []FileHash response (the larger of the two frames in the
// round trip) must fit MaxControlFrameSize. If the bound is ever raised past
// this, big equal-size trees overflow the response frame and surface as E099.
func TestVerifyResponse_FitsFrame(t *testing.T) {
	resp := make([]wire.FileHash, maxVerifyIDsPerFrame)
	for i := range resp {
		// Worst-case payload: all-0xFF root bytes (gob's most expensive
		// varint encoding) and a large index.
		var h [32]byte
		for j := range h {
			h[j] = 0xFF
		}
		resp[i] = wire.FileHash{Index: uint32(i), Hash: h}
	}
	var buf bytes.Buffer
	if err := wire.WriteControl(&buf, wire.TypeVerifyResponse, resp); err != nil {
		t.Fatalf("response for %d hashes does not fit a control frame: %v", maxVerifyIDsPerFrame, err)
	}
	if buf.Len() > wire.MaxControlFrameSize {
		t.Fatalf("response body %d > MaxControlFrameSize %d", buf.Len(), wire.MaxControlFrameSize)
	}
}

// TestResolveVerify_BatchesLargeCandidateSet pins the A2 fix: a same-size tree
// with more candidates than fit in one control frame must be requested in
// multiple bounded VERIFY_REQUEST frames, each answered by a response that
// also fits the frame cap (rather than overflowing and surfacing the honest
// input as E099).
func TestResolveVerify_BatchesLargeCandidateSet(t *testing.T) {
	const n = 2*maxVerifyIDsPerFrame + 7

	recvCtrl, sendCtrl := net.Pipe()
	defer func() { _ = recvCtrl.Close() }()
	defer func() { _ = sendCtrl.Close() }()

	var mu sync.Mutex
	var batches [][]uint32
	var done = make(chan struct{})

	// Peer: answer each VERIFY_REQUEST with a realistically sized response
	// (one FileHash per requested index — the frame that actually overflowed),
	// recording the requested indices so the test can assert the batch shape.
	go func() {
		defer close(done)
		for {
			ft, body, err := wire.ReadControlRaw(sendCtrl)
			if err != nil {
				return // pipe closed: test finished
			}
			if ft != wire.TypeVerifyRequest {
				t.Errorf("peer: unexpected frame %v", ft)
				return
			}
			var batch []uint32
			if err := wire.Decode(body, &batch); err != nil {
				t.Errorf("peer: decode request: %v", err)
				return
			}
			mu.Lock()
			batches = append(batches, batch)
			mu.Unlock()
			resp := make([]wire.FileHash, len(batch))
			for i, idx := range batch {
				var h [32]byte
				for j := range h {
					h[j] = 0xFF // worst-case gob size for the frame-cap check
				}
				resp[i] = wire.FileHash{Index: idx, Hash: h}
			}
			if err := wire.WriteControl(sendCtrl, wire.TypeVerifyResponse, resp); err != nil {
				t.Errorf("peer: response for %d hashes overflowed the frame: %v", len(batch), err)
				return
			}
		}
	}()

	// Plans all flagged for verification. The target files don't exist, so
	// resolveVerify falls through to dispDiffers after the (realistic) hash
	// lookup — the batching, not the verdict, is what's under test.
	plans := make([]entryPlan, n)
	for i := range plans {
		plans[i] = entryPlan{entry: wire.ListingEntry{Index: uint32(i)}, disp: dispVerify}
	}

	s := &Streams{Control: recvCtrl}
	if err := resolveVerify(s, plans); err != nil {
		t.Fatalf("resolveVerify: %v", err)
	}
	_ = recvCtrl.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("peer goroutine did not finish")
	}

	mu.Lock()
	defer mu.Unlock()

	wantBatches := (n + maxVerifyIDsPerFrame - 1) / maxVerifyIDsPerFrame
	if len(batches) != wantBatches {
		t.Fatalf("sent %d VERIFY_REQUEST frames, want %d", len(batches), wantBatches)
	}
	seen := 0
	for i, b := range batches {
		if len(b) == 0 || len(b) > maxVerifyIDsPerFrame {
			t.Errorf("batch %d size %d, want 1..%d", i, len(b), maxVerifyIDsPerFrame)
		}
		for j, idx := range b {
			if idx != uint32(seen+j) {
				t.Fatalf("batch %d entry %d = %d, want %d (order mismatch)", i, j, idx, seen+j)
			}
		}
		seen += len(b)
	}
	if seen != n {
		t.Errorf("requested %d indices across batches, want %d", seen, n)
	}
	for _, p := range plans {
		if p.disp != dispDiffers {
			t.Fatalf("plan %d disp = %v, want dispDiffers (nonexistent target)", p.entry.Index, p.disp)
		}
	}
}
