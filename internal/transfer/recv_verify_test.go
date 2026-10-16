package transfer

import (
	"net"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/wire"
)

// TestResolveVerify_BatchesLargeCandidateSet pins the A2 fix: a same-size tree
// with more candidates than fit in one control frame must be requested in
// multiple bounded VERIFY_REQUEST frames rather than overflowing the 64 KiB
// frame cap (which surfaced the honest input as E099).
func TestResolveVerify_BatchesLargeCandidateSet(t *testing.T) {
	const n = 2*maxVerifyIDsPerFrame + 7

	recvCtrl, sendCtrl := net.Pipe()
	defer func() { _ = recvCtrl.Close() }()
	defer func() { _ = sendCtrl.Close() }()

	var mu sync.Mutex
	var batches [][]uint32
	var done = make(chan struct{})

	// Peer: answer each VERIFY_REQUEST with an empty response, recording the
	// requested indices so the test can assert the batching shape.
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
			if err := wire.WriteControl(sendCtrl, wire.TypeVerifyResponse, []wire.FileHash{}); err != nil {
				return
			}
		}
	}()

	// Plans all flagged for verification; no files needed because the empty
	// response makes every plan fall through to dispDiffers without hashing.
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
			t.Fatalf("plan %d disp = %v, want dispDiffers (empty response)", p.entry.Index, p.disp)
		}
	}
}
