package transfer

import (
	"testing"

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
		{entry: wire.ListingEntry{RelativePath: "link", Type: wire.EntrySymlink, SymlinkTarget: "new.bin"}, disp: dispNew},
	}
	s := summarize(plans)

	// Directory excluded; everything else (incl. symlink) kept, in order.
	want := []SummaryEntry{
		{RelativePath: "new.bin", Size: 100, Status: "new", Type: wire.EntryFile},
		{RelativePath: "same.bin", Size: 200, Status: "identical", Type: wire.EntryFile},
		{RelativePath: "diff.bin", Size: 300, Status: "differs", Type: wire.EntryFile},
		{RelativePath: "part.bin", Size: 400, Status: "resume", Type: wire.EntryFile},
		{RelativePath: "link", Size: 0, Status: "new", Type: wire.EntrySymlink, SymlinkTarget: "new.bin"},
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
	// NewItems counts new + resume + the dir + the symlink (all dispNew/resume).
	if s.Total != 6 || s.NewItems != 4 || s.Identical != 1 || s.Differing != 1 {
		t.Errorf("counts drifted: %+v", s)
	}
	if s.OfferedBytes != 1000 { // 100+200+300+400 (+0 dir +0 link)
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
