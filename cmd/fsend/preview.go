package main

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

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
//   - from is a *followed* symlink's origin (a sender-side annotation); the row
//     has a real size and renders "name (→ target)".
type previewItem struct {
	name string
	size uint64
	note string
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
	if len(items) == previewRows+1 {
		// "… and 1 more" costs the row it hides: print it instead.
		shown = len(items)
	}
	// Name first (the row's subject, default text), then the size and any
	// note as faint detail — except "differs", the one note that needs a
	// decision. Names pad to one column, capped so a single long path
	// doesn't push every size off screen.
	nameWidth, sizeWidth := 0, 0
	for _, it := range items[:shown] {
		nameWidth = max(nameWidth, min(nameColumnCap, utf8.RuneCountInString(rowName(it))))
		sizeWidth = max(sizeWidth, len(sizeCell(it)))
	}
	for _, it := range items[:shown] {
		name := rowName(it)
		gap := strings.Repeat(" ", max(0, nameWidth-utf8.RuneCountInString(name)))
		line := pad + uxlog.Path(name) + gap + "   " + uxlog.Muted(fmt.Sprintf("%*s", sizeWidth, sizeCell(it)))
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
		_, _ = fmt.Fprintln(w, pad+uxlog.Muted(fmt.Sprintf("… and %d more (%s)", more, uxlog.HumanBytes(int64(rest)))))
	}
}

// nameColumnCap bounds the padded name column of a file list.
const nameColumnCap = 40

// rowName is a row's name cell, with a followed symlink's origin (path
// material too): "name (→ target)".
func rowName(it previewItem) string {
	if it.from != "" {
		return it.name + " (→ " + it.from + ")"
	}
	return it.name
}

// sizeCell renders the size column for one row.
func sizeCell(it previewItem) string {
	return uxlog.HumanBytes(int64(it.size))
}

// noteText colours a status tag: "differs" needs a decision, so it wears
// the warning colour; the rest ("up to date", "resume") are faint — rows
// that need no attention recede.
func noteText(note string) string {
	if note == "differs" {
		return uxlog.Alert(note)
	}
	return uxlog.Muted(note)
}

// stripCommonDir removes the longest leading directory prefix shared by every
// row (e.g. the wrapping "proj/" already shown in the headline), leaving the
// part that distinguishes each row, and returns that prefix ("" when nothing
// was stripped). A no-op when the rows don't share a root (multi-path or
// contents sends). Only whole path segments are stripped, and never a
// basename.
func stripCommonDir(items []previewItem) string {
	if len(items) < 2 {
		return ""
	}
	segs := strings.Split(items[0].name, "/")
	if len(segs) < 2 {
		return "" // first row has no directory component
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
			return ""
		}
	}
	stripped := strings.Join(prefix, "/") + "/"
	cut := len(stripped) // include the trailing slash
	for i := range items {
		if len(items[i].name) > cut {
			items[i].name = items[i].name[cut:]
		}
	}
	return stripped
}
