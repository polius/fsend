package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/wire"
)

// render captures renderPreview into a string for assertion.
func render(items []previewItem) string {
	var b bytes.Buffer
	renderPreview(&b, items, 6)
	return b.String()
}

func TestRenderPreview_SingleItemPrintsNothing(t *testing.T) {
	if got := render([]previewItem{{name: "solo.bin", size: 100}}); got != "" {
		t.Errorf("single item should print nothing, got %q", got)
	}
	if got := render(nil); got != "" {
		t.Errorf("empty should print nothing, got %q", got)
	}
}

func TestRenderPreview_LargestFirst(t *testing.T) {
	got := render([]previewItem{
		{name: "small.txt", size: 5},
		{name: "big.bin", size: 4_000_000_000},
		{name: "mid.bin", size: 1_000_000},
	})
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 rows, got %d:\n%s", len(lines), got)
	}
	if !strings.Contains(lines[0], "big.bin") ||
		!strings.Contains(lines[1], "mid.bin") ||
		!strings.Contains(lines[2], "small.txt") {
		t.Errorf("rows not sorted largest-first:\n%s", got)
	}
}

func TestRenderPreview_TruncatesWithHonestRemainder(t *testing.T) {
	// 12 files of 10 bytes each → 10 shown + "… and 2 more (20 B)".
	var items []previewItem
	for i := 0; i < 12; i++ {
		items = append(items, previewItem{name: "f", size: 10})
	}
	got := render(items)
	if n := strings.Count(got, "\n"); n != previewRows+1 {
		t.Errorf("want %d rows + remainder, got %d lines:\n%s", previewRows, n, got)
	}
	if !strings.Contains(got, "… and 2 more (20 B)") {
		t.Errorf("remainder line missing or wrong total:\n%s", got)
	}
}

func TestRenderPreview_NoteRendered(t *testing.T) {
	got := render([]previewItem{
		{name: "a.bin", size: 200, note: "up to date"},
		{name: "b.bin", size: 100, note: "differs"},
	})
	if !strings.Contains(got, "a.bin") || !strings.Contains(got, "up to date") {
		t.Errorf("note not rendered:\n%s", got)
	}
	if !strings.Contains(got, "differs") {
		t.Errorf("differs note missing:\n%s", got)
	}
}

func TestSenderPreview_DropsDirectories(t *testing.T) {
	sources := []transfer.Source{
		{Entry: wire.ListingEntry{RelativePath: "proj", Type: wire.EntryDir}},
		{Entry: wire.ListingEntry{RelativePath: "proj/a.bin", Size: 10, Type: wire.EntryFile}},
		{Entry: wire.ListingEntry{RelativePath: "proj/link", Type: wire.EntrySymlink}},
	}
	items := senderPreview(sources)
	if len(items) != 2 {
		t.Fatalf("want 2 non-dir items, got %d: %+v", len(items), items)
	}
	for _, it := range items {
		if it.name == "proj" {
			t.Errorf("directory should be dropped: %+v", items)
		}
	}
}

func TestReceiverPreview_StatusMapping(t *testing.T) {
	items := receiverPreview([]transfer.SummaryEntry{
		{RelativePath: "new.bin", Size: 10, Status: "new"},
		{RelativePath: "same.bin", Size: 20, Status: "identical"},
		{RelativePath: "diff.bin", Size: 30, Status: "differs"},
		{RelativePath: "part.bin", Size: 40, Status: "resume"},
	})
	want := map[string]string{"new.bin": "", "same.bin": "up to date", "diff.bin": "differs", "part.bin": "resume"}
	for _, it := range items {
		if want[it.name] != it.note {
			t.Errorf("%s: want note %q, got %q", it.name, want[it.name], it.note)
		}
	}
}

func TestReceiverPreview_Symlink(t *testing.T) {
	items := receiverPreview([]transfer.SummaryEntry{
		{RelativePath: "link", Status: "new", Type: wire.EntrySymlink, SymlinkTarget: "target.bin"},
	})
	if items[0].link != "target.bin" {
		t.Errorf("symlink target not carried: %+v", items[0])
	}
}

func TestRenderPreview_StripsCommonDir(t *testing.T) {
	got := render([]previewItem{
		{name: "proj/sub/a.bin", size: 30},
		{name: "proj/b.bin", size: 20},
		{name: "proj/c.bin", size: 10},
	})
	if strings.Contains(got, "proj/") {
		t.Errorf("common 'proj/' prefix should be stripped:\n%s", got)
	}
	for _, want := range []string{"sub/a.bin", "b.bin", "c.bin"} {
		if !strings.Contains(got, want) {
			t.Errorf("row %q missing after strip:\n%s", want, got)
		}
	}
}

// No common root (multi-path send) → nothing stripped.
func TestRenderPreview_NoCommonDirNoOp(t *testing.T) {
	got := render([]previewItem{
		{name: "alpha/a.bin", size: 20},
		{name: "beta/b.bin", size: 10},
	})
	if !strings.Contains(got, "alpha/a.bin") || !strings.Contains(got, "beta/b.bin") {
		t.Errorf("unrelated roots must be preserved:\n%s", got)
	}
}

func TestRenderPreview_SymlinkRow(t *testing.T) {
	got := render([]previewItem{
		{name: "big.bin", size: 1000},
		{name: "latest", link: "big.bin"},
	})
	if !strings.Contains(got, "latest → big.bin") {
		t.Errorf("symlink should render as 'name → target':\n%s", got)
	}
	if strings.Contains(got, "0 B   latest") {
		t.Errorf("symlink must not render a byte size:\n%s", got)
	}
}

// Under --overwrite the differing files transfer too, so the bar must be
// sized for them upfront (BytesToRecv + DifferingBytes); without it the bar
// reads past 100%. The default (no overwrite) keeps them out of the total.
func TestAccept_BarSizing(t *testing.T) {
	sum := transfer.ClassifySummary{
		Total: 2, NewItems: 1, Differing: 1,
		BytesToRecv: 100, OfferedBytes: 1000, DifferingBytes: 900,
	}
	overwrite := newReceiverUI(context.Background(), &flags{yes: true, overwrite: true}, "/tmp", false, mustLANInfo())
	_ = overwrite.accept(filesHello(), sum)
	if overwrite.bytesHint != 1000 {
		t.Errorf("--overwrite bytesHint = %d, want 1000 (net + differing)", overwrite.bytesHint)
	}
	keep := newReceiverUI(context.Background(), &flags{yes: true}, "/tmp", false, mustLANInfo())
	_ = keep.accept(filesHello(), sum)
	if keep.bytesHint != 100 {
		t.Errorf("default bytesHint = %d, want 100 (net only; differing kept)", keep.bytesHint)
	}
}

// For a contents/multi-path send the peer's display name is itself the count
// ("2 files"); the headline must not repeat it as "2 files · 2 files".
func TestPromptAccept_HeadlineNoDuplicateCount(t *testing.T) {
	h := wire.SenderHello{Mode: wire.ModeFiles, DisplayName: "2 files"}
	sum := transfer.ClassifySummary{
		Total: 2, NewItems: 2, BytesToRecv: 100, OfferedBytes: 100,
		Files: []transfer.SummaryEntry{
			{RelativePath: "a.bin", Size: 60, Status: "new", Type: wire.EntryFile},
			{RelativePath: "b.bin", Size: 40, Status: "new", Type: wire.EntryFile},
		},
	}
	got := captureStderr(t, func() {
		ui := newReceiverUI(context.Background(), &flags{yes: true}, "/tmp", false, mustLANInfo())
		_ = ui.promptAccept(h, sum)
	})
	if strings.Contains(got, "2 files  ·  2 files") {
		t.Errorf("headline duplicates the count:\n%s", got)
	}
	if !strings.Contains(got, "2 files  ·  100 B") {
		t.Errorf("expected collapsed 'count · size' headline:\n%s", got)
	}
}

func TestReceiverSizeClause(t *testing.T) {
	cases := []struct {
		name string
		s    transfer.ClassifySummary
		want string
	}{
		{"all new", transfer.ClassifySummary{OfferedBytes: 1_200_000_000, BytesToRecv: 1_200_000_000}, "1.2 GB"},
		{"some skipped", transfer.ClassifySummary{OfferedBytes: 1_200_000_000, BytesToRecv: 307_000_000}, "307 MB of 1.2 GB"},
		{"all up to date", transfer.ClassifySummary{OfferedBytes: 1_200_000_000, BytesToRecv: 0, Identical: 3}, "already up to date"},
		{"single differ", transfer.ClassifySummary{OfferedBytes: 4_200_000_000, BytesToRecv: 0, Differing: 1}, "4.2 GB"},
		// net and offered round to the same human string → show one number,
		// not "1.2 GB of 1.2 GB".
		{"negligible skip", transfer.ClassifySummary{OfferedBytes: 1_200_000_000, BytesToRecv: 1_199_600_000}, "1.2 GB"},
	}
	for _, c := range cases {
		if got := receiverSizeClause(c.s); got != c.want {
			t.Errorf("%s: receiverSizeClause = %q, want %q", c.name, got, c.want)
		}
	}
}

// Peer-supplied names must be sanitized like every other untrusted string we
// print — a bidi override must not survive into the preview.
func TestReceiverPreview_SanitizesNames(t *testing.T) {
	items := receiverPreview([]transfer.SummaryEntry{
		{RelativePath: "evil\u202ename.bin", Size: 10, Status: "new"},
	})
	if strings.ContainsRune(items[0].name, '\u202e') {
		t.Errorf("bidi override survived sanitization: %q", items[0].name)
	}
}

// The sender artifact block lists the files largest-first under the headline.
func TestPrintSendArtifact_ListsFiles(t *testing.T) {
	plan := &sendPlan{
		mode: wire.ModeFiles, displayName: "proj/", label: "proj/",
		totalFiles: 2, totalBytes: 30,
		sources: []transfer.Source{
			{Entry: wire.ListingEntry{RelativePath: "proj/small.txt", Size: 10, Type: wire.EntryFile}},
			{Entry: wire.ListingEntry{RelativePath: "proj/big.bin", Size: 20, Type: wire.EntryFile}},
		},
	}
	got := captureStderr(t, func() {
		sp := printSendArtifact(&flags{}, "abc-defg-hij", plan)
		if sp != nil {
			sp.Stop()
		}
	})
	// The wrapping "proj/" is stripped (it's in the headline); rows show the
	// distinguishing path.
	if strings.Contains(got, "proj/big.bin") {
		t.Errorf("common 'proj/' prefix should be stripped from rows:\n%s", got)
	}
	if !strings.Contains(got, "big.bin") || !strings.Contains(got, "small.txt") {
		t.Errorf("artifact missing file rows:\n%s", got)
	}
	if strings.Index(got, "big.bin") > strings.Index(got, "small.txt") {
		t.Errorf("files not largest-first in artifact:\n%s", got)
	}
}

// The accept prompt lists the incoming files with their per-file status.
func TestPromptAccept_ListsFiles(t *testing.T) {
	sum := transfer.ClassifySummary{
		Total: 2, NewItems: 1, Differing: 1,
		BytesToRecv: 10, OfferedBytes: 30, DifferingBytes: 20,
		Files: []transfer.SummaryEntry{
			{RelativePath: "a.bin", Size: 10, Status: "new", Type: wire.EntryFile},
			{RelativePath: "b.bin", Size: 20, Status: "differs", Type: wire.EntryFile},
		},
	}
	got := captureStderr(t, func() {
		ui := newReceiverUI(context.Background(), &flags{yes: true}, "/tmp", false, mustLANInfo())
		_ = ui.promptAccept(filesHello(), sum)
	})
	if !strings.Contains(got, "a.bin") || !strings.Contains(got, "b.bin") {
		t.Errorf("accept prompt missing file rows:\n%s", got)
	}
	if !strings.Contains(got, "differs") {
		t.Errorf("accept prompt missing per-file status:\n%s", got)
	}
	// Headline: file count (not items), "X of Y", and the differ count.
	for _, want := range []string{"2 files", "10 B of 30 B", "1 differ"} {
		if !strings.Contains(got, want) {
			t.Errorf("headline missing %q:\n%s", want, got)
		}
	}
}
