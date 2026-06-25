package main

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/polius/fsend/internal/uxlog"
)

// previewRows is how many files the pre-transfer preview lists before
// collapsing the rest into a "… and N more" line. The rows shown are the
// largest, so they're the ones that dominate the transfer.
const previewRows = 10

// previewItem is one row of the pre-transfer file list.
//
//   - note is a status annotation ("up to date", "differs", "resume"); ""
//     omits it. "differs" is the one that warrants attention, so it's the
//     only one rendered in colour rather than dimmed.
//   - link is a *preserved* symlink target (from an older sender); non-empty
//     renders "name → target" with "→" in the size column (no byte size).
//   - from is a *followed* symlink's origin (a sender-side annotation); the row
//     has a real size and renders "name (→ target)". link and from are mutually
//     exclusive.
type previewItem struct {
	name string
	size uint64
	note string
	link string
	from string
}

// renderPreview writes a size-sorted, truncated file list to w, indented to
// sit under the artifact summary line. Names must already be sanitized for
// display. Single-item lists print nothing — the summary line already names
// the file. The wrapping directory shared by every row is stripped (it's in
// the headline), and sizes are right-aligned so the list reads as a manifest.
func renderPreview(w io.Writer, items []previewItem, indent int) {
	if len(items) <= 1 {
		return
	}
	stripCommonDir(items)
	// Largest first; ties broken by name so the output is deterministic.
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].size != items[j].size {
			return items[i].size > items[j].size
		}
		return items[i].name < items[j].name
	})

	pad := strings.Repeat(" ", indent)
	shown := min(len(items), previewRows)
	sizeWidth := 0
	for _, it := range items[:shown] {
		if n := len(sizeCell(it)); n > sizeWidth {
			sizeWidth = n
		}
	}
	for _, it := range items[:shown] {
		name := it.name
		switch {
		case it.link != "": // preserved symlink: "name → target", "→" size cell
			name += " → " + it.link
		case it.from != "": // followed symlink: real size + "name (→ target)"
			name += " (→ " + it.from + ")"
		}
		line := fmt.Sprintf("%s%*s   %s", pad, sizeWidth, sizeCell(it), name)
		if it.note != "" {
			line += "   " + noteText(it.note)
		}
		_, _ = fmt.Fprintln(w, line)
	}
	if more := len(items) - shown; more > 0 {
		var rest uint64
		for _, it := range items[shown:] {
			rest += it.size
		}
		_, _ = fmt.Fprintf(w, "%s%s\n", pad,
			uxlog.Dim(fmt.Sprintf("… and %d more (%s)", more, uxlog.HumanBytes(int64(rest)))))
	}
}

// sizeCell renders the size column for one row: a byte count, or "→" for a
// symlink (which is a pointer, not a zero-byte file).
func sizeCell(it previewItem) string {
	if it.link != "" {
		return "→"
	}
	return uxlog.HumanBytes(int64(it.size))
}

// noteText colours a status tag: "differs" needs a decision, so it's
// highlighted; the rest are reassuring background, so they're dimmed.
func noteText(note string) string {
	if note == "differs" {
		return uxlog.Alert(note)
	}
	return uxlog.Dim(note)
}

// stripCommonDir removes the longest leading directory prefix shared by every
// row (e.g. the wrapping "proj/" already shown in the headline), leaving the
// part that distinguishes each row. A no-op when the rows don't share a root
// (multi-path or contents sends). Only whole path segments are stripped, and
// never a basename.
func stripCommonDir(items []previewItem) {
	if len(items) < 2 {
		return
	}
	segs := strings.Split(items[0].name, "/")
	if len(segs) < 2 {
		return // first row has no directory component
	}
	prefix := segs[:len(segs)-1] // candidate: item0's directories
	for _, it := range items[1:] {
		s := strings.Split(it.name, "/")
		n := 0
		for n < len(prefix) && n < len(s)-1 && prefix[n] == s[n] {
			n++
		}
		prefix = prefix[:n]
		if len(prefix) == 0 {
			return
		}
	}
	cut := len(strings.Join(prefix, "/")) + 1 // include the trailing slash
	for i := range items {
		if len(items[i].name) > cut {
			items[i].name = items[i].name[cut:]
		}
	}
}
