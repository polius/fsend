package uxlog

import (
	"regexp"
	"strings"

	"github.com/mattn/go-runewidth"
)

// cells measures text in terminal columns, not runes: a CJK ideograph or
// an emoji takes two, a combining mark none. Every line that is clipped or
// padded to a width goes through it, so a device or file name in a wide
// script can't push a redrawn line past the terminal's edge.
//
// It is the measure the progress bar (mpb) already uses, so the name chip
// and the row around it agree. Like mpb it follows go-runewidth's locale
// rule for the East Asian Ambiguous class (… · → ━): one column, or two in
// a CJK locale, where some terminals draw them wide. Counting two where
// the terminal draws one only clips a line a little early; the reverse
// would let it wrap. RUNEWIDTH_EASTASIAN=0|1 overrides the guess.
var cells = runewidth.DefaultCondition

// Width returns the number of terminal columns s occupies. s must be
// plain text: escape sequences are not skipped.
func Width(s string) int { return cells.StringWidth(s) }

// escapes matches the CSI (colour) and OSC (link, title) sequences a styled
// string may carry.
var escapes = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)

// visibleWidth is Width for styled text: escape sequences take no columns.
func visibleWidth(s string) int {
	if strings.IndexByte(s, 0x1b) < 0 {
		return Width(s)
	}
	return Width(escapes.ReplaceAllString(s, ""))
}

// Clip cuts s to at most max columns, ending in "…" when cut. A wide
// character that would straddle the limit is dropped whole, so the result
// can be a column short of max, never over it.
func Clip(s string, max int) string {
	ell := Width("…")
	switch {
	case Width(s) <= max:
		return s
	case max <= 0:
		return ""
	case max <= ell:
		return clipHead("…", max)
	}
	return strings.TrimRight(clipHead(s, max-ell), " ") + "…"
}

// PadRight pads s with spaces to width columns; a longer s is returned
// as it is.
func PadRight(s string, width int) string {
	if n := width - Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

// clipHead returns the longest prefix of s that fits in n columns, never
// splitting a grapheme cluster.
func clipHead(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return cells.Truncate(s, n, "")
}

// clipTail returns the longest suffix of s that fits in n columns, never
// splitting a grapheme cluster.
func clipTail(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return cells.TruncatePrefix(s, n, "")
}
