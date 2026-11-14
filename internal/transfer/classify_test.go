package transfer

import (
	"errors"
	"math"
	"os"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// summarize must surface a per-file row for every non-directory entry, with
// the status the preview annotates rows with, while still tallying the counts
// the breakdown line uses.
func TestSummarize_PopulatesFiles(t *testing.T) {
	plans := []entryPlan{
		{entry: wire.ListingEntry{RelativePath: "dir", Type: wire.EntryDir}, disp: dispNew},
		{entry: wire.ListingEntry{RelativePath: "new.bin", Size: 100, Type: wire.EntryFile}, disp: dispNew},
		{entry: wire.ListingEntry{RelativePath: "same.bin", Size: 200, Type: wire.EntryFile}, disp: dispIdentical},
		{entry: wire.ListingEntry{RelativePath: "diff.bin", Size: 300, Type: wire.EntryFile}, disp: dispDiffers},
		{entry: wire.ListingEntry{RelativePath: "part.bin", Size: 400, Type: wire.EntryFile}, disp: dispResume, resumeOffset: 150},
	}
	s := summarize(plans)

	// Directory excluded; every file kept, in order.
	want := []SummaryEntry{
		{RelativePath: "new.bin", Size: 100, Status: "new", Type: wire.EntryFile},
		{RelativePath: "same.bin", Size: 200, Status: "identical", Type: wire.EntryFile},
		{RelativePath: "diff.bin", Size: 300, Status: "differs", Type: wire.EntryFile},
		{RelativePath: "part.bin", Size: 400, Status: "resume", Type: wire.EntryFile},
	}
	if len(s.Files) != len(want) {
		t.Fatalf("want %d file rows (dir dropped), got %d: %+v", len(want), len(s.Files), s.Files)
	}
	for i, w := range want {
		if s.Files[i] != w {
			t.Errorf("Files[%d] = %+v, want %+v", i, s.Files[i], w)
		}
	}
	// Counts reconcile, and the byte tallies separate offered (everything)
	// from what actually moves.
	// NewItems counts new + resume + the dir (all dispNew/resume).
	if s.Total != 5 || s.NewItems != 3 || s.Identical != 1 || s.Differing != 1 {
		t.Errorf("counts drifted: %+v", s)
	}
	if s.OfferedBytes != 1000 { // 100+200+300+400 (+0 dir)
		t.Errorf("OfferedBytes = %d, want 1000", s.OfferedBytes)
	}
	if s.DifferingBytes != 300 {
		t.Errorf("DifferingBytes = %d, want 300", s.DifferingBytes)
	}
	// BytesToRecv = new (100) + resume remaining (400-150=250) = 350.
	if s.BytesToRecv != 350 {
		t.Errorf("BytesToRecv = %d, want 350", s.BytesToRecv)
	}
}

// The Kept flag must mark exactly the skips of differing/conflicting entries
// the receiver kept — it's what lets the sender report "kept by receiver"
// instead of a false "✓ Sent" when nothing was delivered.
func TestPlanToDecision_KeptFlag(t *testing.T) {
	for _, tc := range []struct {
		disp     disposition
		approve  bool
		wantAct  wire.DecisionAction
		wantKept bool
	}{
		{dispNew, false, wire.DecisionSend, false},
		{dispIdentical, false, wire.DecisionSkip, false},
		{dispDiffers, false, wire.DecisionSkip, true},
		{dispConflict, false, wire.DecisionSkip, true},
		{dispDiffers, true, wire.DecisionSend, false},
		{dispConflict, true, wire.DecisionSend, false},
	} {
		p := entryPlan{entry: wire.ListingEntry{Type: wire.EntryFile}, disp: tc.disp}
		d := planToDecision(&p, tc.approve, RecvOptions{})
		if d.Action != tc.wantAct || d.Kept != tc.wantKept {
			t.Errorf("planToDecision(%v, approve=%v) = {Action:%v Kept:%v}, want {%v %v}",
				tc.disp, tc.approve, d.Action, d.Kept, tc.wantAct, tc.wantKept)
		}
	}
}

// Only files and folders are accepted: the retired symlink type (2) and
// any type this protocol doesn't define fail the whole listing as unsafe
// (E012), before anything is created.
func TestClassify_RefusesUnknownEntryTypes(t *testing.T) {
	for _, typ := range []wire.EntryType{2, 3, 255} {
		dir := t.TempDir()
		_, err := classify([]wire.ListingEntry{
			{Index: 0, RelativePath: "ok.txt", Size: 1, Type: wire.EntryFile},
			{Index: 1, RelativePath: "odd", Type: typ},
		}, dir, false)
		if !errors.Is(err, fserrors.ErrPathTraversal) {
			t.Errorf("type %d: err = %v, want ErrPathTraversal", typ, err)
		}
		if ents, _ := os.ReadDir(dir); len(ents) != 0 {
			t.Errorf("type %d: the receive dir is not empty: %v", typ, ents)
		}
	}
}

// A listing whose sizes don't fit int64 — one entry, or the sum — is a
// protocol error: the receiver's offsets, truncates and totals are int64.
func TestValidateListing_SizesFitInt64(t *testing.T) {
	ok := []wire.ListingEntry{{Index: 0, Size: math.MaxInt64}}
	if err := validateListing(ok); err != nil {
		t.Errorf("one MaxInt64 entry: %v", err)
	}
	for name, es := range map[string][]wire.ListingEntry{
		"entry": {{Index: 0, Size: math.MaxInt64 + 1}},
		"sum":   {{Index: 0, Size: math.MaxInt64}, {Index: 1, Size: 1}},
		"wraps": {{Index: 0, Size: math.MaxUint64}, {Index: 1, Size: 2}},
	} {
		if err := validateListing(es); !errors.Is(err, fserrors.ErrProtocolError) {
			t.Errorf("%s: err = %v, want ErrProtocolError", name, err)
		}
	}
}
