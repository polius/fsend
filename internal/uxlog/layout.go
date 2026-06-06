package uxlog

import (
	"os"
	"strings"

	"golang.org/x/term"
)

// Separator returns a box-drawing horizontal rule sized to fit the
// current terminal — at most 60 columns to keep blocks scannable on
// wide terminals, at least 20 to stay useful on narrow ones.
//
// In non-TTY contexts (pipes, logs) we fall back to an ASCII-only rule
// at the default width so grepping log files works without funky
// Unicode column-counting.
func Separator() string {
	w := termWidth()
	if w > 60 {
		w = 60
	}
	if w < 20 {
		w = 20
	}
	if IsTTY(os.Stderr) {
		return strings.Repeat("─", w)
	}
	return strings.Repeat("-", w)
}

func termWidth() int {
	if !IsTTY(os.Stderr) {
		return 45
	}
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w <= 0 {
		return 45
	}
	return w
}
