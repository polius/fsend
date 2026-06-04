package main

import (
	"io"
	"os"
)

// isTTY reports whether w refers to a terminal (rather than a piped file).
//
// We use this to decide between unicode/ANSI output and plain ASCII. Cobra
// has its own helpers but we keep a tiny dependency-free check here so the
// rest of the rendering code doesn't have to plumb cobra everywhere.
func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
