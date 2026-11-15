package uxlog

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// narrowCells pins the width rule to the one every current terminal uses
// (East Asian Ambiguous characters one column wide), whatever locale the
// tests run under.
func narrowCells(t *testing.T) {
	t.Helper()
	old := cells
	cells = &runewidth.Condition{EastAsianWidth: false, StrictEmojiNeutral: true}
	t.Cleanup(func() { cells = old })
}

// wideCells is the rule a CJK locale selects: ambiguous characters (… ·)
// count two columns.
func wideCells(t *testing.T) {
	t.Helper()
	old := cells
	cells = &runewidth.Condition{EastAsianWidth: true, StrictEmojiNeutral: true}
	t.Cleanup(func() { cells = old })
}

// Names a terminal draws wider or narrower than their rune count.
const (
	cjkDevice   = "田中さんのノートパソコン" // 12 runes, 24 columns
	cjkFile     = "写真/2026年の旅行/富士山の夜明け.jpeg"
	emojiDevice = "💻 Pol's MacBook 🚀"
	hangulFile  = "문서/회의록-최종본.docx"
	combining   = "Zoe\u0308's cafe\u0301" // two combining marks: 10 columns
)

func TestWidth(t *testing.T) {
	narrowCells(t)
	for _, c := range []struct {
		s    string
		want int
	}{
		{"", 0},
		{"report.pdf", 10},
		{"日本語", 6},            // East Asian Wide
		{"ＡＢＣ", 6},            // East Asian Fullwidth
		{"ｱｲｳ", 3},            // halfwidth katakana
		{"한국어", 6},            // Hangul syllables
		{"e\u0301", 1},        // e + combining acute
		{combining, 10},       // marks take no column
		{"👍", 2},              // emoji
		{"👨‍👩‍👧", 2},          // a ZWJ sequence is one glyph
		{"🇯🇵", 2},             // a flag is one glyph
		{"a\u200bb", 2},       // zero-width space
		{"… · → ━ ✓", 9},      // the UI's own symbols: one column each
		{cjkDevice, 24},       // 12 runes
		{"Pol の MacBook", 14}, // mixed
	} {
		if got := Width(c.s); got != c.want {
			t.Errorf("Width(%q) = %d, want %d (%d runes)", c.s, got, c.want, utf8.RuneCountInString(c.s))
		}
	}
}

func TestClip(t *testing.T) {
	narrowCells(t)
	for _, c := range []struct {
		s    string
		max  int
		want string
	}{
		{"short", 10, "short"},
		{"exactly-10", 10, "exactly-10"},
		{"Waiting for receiver", 8, "Waiting…"},
		{"Waiting for receiver", 9, "Waiting…"}, // the space before the cut goes too
		{"日本語のファイル", 7, "日本語…"},                 // 3 wide + …
		{"日本語のファイル", 6, "日本…"},                  // a wide character never straddles the limit
		{"日本語のファイル", 16, "日本語のファイル"},
		{"abc", 1, "…"},
		{"abc", 0, ""},
		{"abc", -3, ""},
	} {
		got := Clip(c.s, c.max)
		if got != c.want {
			t.Errorf("Clip(%q, %d) = %q, want %q", c.s, c.max, got, c.want)
		}
		if w := Width(got); w > max(c.max, 0) {
			t.Errorf("Clip(%q, %d) = %q is %d columns", c.s, c.max, got, w)
		}
	}
}

// Whatever the text and the budget, a clip never exceeds the budget, never
// splits a character, and under either width rule.
func TestClipNeverOverflows(t *testing.T) {
	for _, rule := range []func(*testing.T){narrowCells, wideCells} {
		rule(t)
		for _, s := range []string{cjkDevice, cjkFile, emojiDevice, hangulFile, combining, "plain-ascii-name.tar.gz", "👨‍👩‍👧👨‍👩‍👧👨‍👩‍👧"} {
			for limit := -1; limit <= Width(s)+2; limit++ {
				got := Clip(s, limit)
				if w := Width(got); w > max(limit, 0) {
					t.Errorf("Clip(%q, %d) = %q is %d columns", s, limit, got, w)
				}
				if !utf8.ValidString(got) {
					t.Errorf("Clip(%q, %d) = %q is not valid UTF-8", s, limit, got)
				}
				if limit >= Width(s) && got != s {
					t.Errorf("Clip(%q, %d) = %q, want it whole", s, limit, got)
				}
			}
		}
	}
}

func TestPadRight(t *testing.T) {
	narrowCells(t)
	for _, c := range []struct {
		s    string
		w    int
		want string
	}{
		{"a.txt", 8, "a.txt   "},
		{"日本語", 8, "日本語  "}, // 6 columns + 2
		{"too-long-name", 4, "too-long-name"},
		{"", 3, "   "},
	} {
		if got := PadRight(c.s, c.w); got != c.want {
			t.Errorf("PadRight(%q, %d) = %q, want %q", c.s, c.w, got, c.want)
		}
	}
	// A column of names in mixed scripts lines up.
	rows := []string{"README.md", "写真.jpeg", "회의록.docx", "e\u0301te\u0301.txt"}
	for _, r := range rows {
		if got := Width(PadRight(r, 12)); got != 12 {
			t.Errorf("PadRight(%q, 12) is %d columns", r, got)
		}
	}
}

// U2: the waiting spinner names the peer's device. With a name in a wide
// script the line must still fit the terminal — a line that wraps leaves a
// row behind on every redraw.
func TestFitSpinnerWideNamesAt40Columns(t *testing.T) {
	for name, rule := range map[string]func(*testing.T){"narrow": narrowCells, "cjk locale": wideCells} {
		rule(t)
		for _, device := range []string{cjkDevice, emojiDevice, "Pol の MacBook Pro", "회의실 노트북", combining, "ｱｲｳｴｵ-PC"} {
			for _, msg := range []string{
				"Waiting for " + device + " to accept",
				"Waiting for " + device + " to enter the password",
				"Waiting for " + device + " to decide",
			} {
				for _, elapsed := range []string{"", "4 s", "1 min 05 s"} {
					for _, width := range []int{40, 60, 80} {
						got := fitSpinner(msg, elapsed, HintCancel, width)
						// The frame glyph and its space lead the line.
						if n := 2 + Width(got); n > width-1 {
							t.Errorf("%s, %d columns: %q is %d columns wide", name, width, got, n)
						}
						if !utf8.ValidString(got) {
							t.Errorf("%s, %d columns: %q is not valid UTF-8", name, width, got)
						}
						if elapsed != "" && !strings.Contains(got, elapsed) {
							t.Errorf("%s, %d columns: %q lost the elapsed time", name, width, got)
						}
					}
				}
			}
		}
	}
}

// The exact cut at 40 columns: 37 are left after the frame and the last
// column; the time keeps its 6, and the 31 that remain hold "Waiting for "
// (12), nine ideographs (18) and the ellipsis.
func TestFitSpinnerCJKExact(t *testing.T) {
	narrowCells(t)
	got := fitSpinner("Waiting for "+cjkDevice+" to accept", "4 s", HintCancel, 40)
	want := "Waiting for 田中さんのノートパ… · 4 s"
	if got != want {
		t.Errorf("fitSpinner = %q (%d columns), want %q", got, Width(got), want)
	}
	// Counted in runes, the old way, the same budget let 18 ideographs
	// through: 36 columns of name alone.
	if Width(got)+2 > 39 {
		t.Errorf("line is %d columns", Width(got)+2)
	}
}

// U2: the file chip on the progress row. Its budget is in columns, so a
// name in a wide script is cut to what the row has.
func TestTruncateNameWideScripts(t *testing.T) {
	for name, rule := range map[string]func(*testing.T){"narrow": narrowCells, "cjk locale": wideCells} {
		rule(t)
		for _, s := range []string{cjkFile, hangulFile, "assets/" + cjkDevice + ".mov", "📷 photos/🏔 sunrise.jpeg", combining + ".txt"} {
			for limit := -1; limit <= Width(s)+1; limit++ {
				got := truncateName(s, limit)
				if w := Width(got); w > max(limit, 0) {
					t.Errorf("%s: truncateName(%q, %d) = %q is %d columns", name, s, limit, got, w)
				}
				if !utf8.ValidString(got) {
					t.Errorf("%s: truncateName(%q, %d) = %q is not valid UTF-8", name, s, limit, got)
				}
				if limit >= Width(s) && got != s {
					t.Errorf("%s: truncateName(%q, %d) = %q, want it whole", name, s, limit, got)
				}
			}
		}
	}
	narrowCells(t)
	// The basename stays whole when it fits, with the head of the path.
	if got, want := truncateName(cjkFile, 30), "写真/2026…/富士山の夜明け.jpeg"; got != want {
		t.Errorf("truncateName(%q, 30) = %q, want %q", cjkFile, got, want)
	}
	// Too long for the budget: cut in the middle, the extension kept.
	if got := truncateName(cjkFile, 16); !strings.HasSuffix(got, ".jpeg") || !strings.Contains(got, "…") {
		t.Errorf("truncateName(%q, 16) = %q, want a middle cut that keeps the extension", cjkFile, got)
	}
}

// The chip as the 40-column row lays it out: separator, name and counter
// stay inside the columns layout booked for them.
func TestFileChipFitsItsBudgetAt40Columns(t *testing.T) {
	narrowCells(t)
	for _, width := range []int{40, 60, 80, 100} {
		for _, total := range []int64{500, 35_000_000, 4_000_000_000} {
			lay := layout(width, total, true)
			if lay.nameCap == 0 {
				continue
			}
			for _, label := range []string{cjkFile, hangulFile, "📷 photos/🏔 sunrise.jpeg", "assets/video1.bin"} {
				p := &Progress{}
				p.SetLabel(label)
				p.SetFilePos(3, 12)
				name, tail := p.chipParts(lay.nameCap)
				if got, budget := Width(name+tail), Width(chipSep)+lay.nameCap; got > budget {
					t.Errorf("width %d, total %d: chip %q is %d columns, budget %d",
						width, total, name+tail, got, budget)
				}
			}
		}
	}
}

// The whole 40-column row with a wide name: every element at its widest
// plus the chip stays inside the terminal.
func TestProgressRowWithWideNameFits(t *testing.T) {
	narrowCells(t)
	for _, width := range []int{40, 50, 60, 80} {
		const total = int64(35_000_000)
		lay := layout(width, total, true)
		used := gutter + lay.bar + pctFurniture
		if lay.counter {
			used += counterFurniture(total)
		}
		if lay.rate {
			used += rateFurniture
		}
		if lay.eta {
			used += etaFurniture
		}
		p := &Progress{}
		p.SetLabel(cjkFile)
		p.SetFilePos(11, 12)
		name, tail := p.chipParts(lay.nameCap)
		used += Width(name + tail)
		if used > width-1 {
			t.Errorf("width %d: the row needs %d columns (%s)", width, used, fmt.Sprintf("%+v", lay))
		}
	}
}
