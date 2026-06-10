package main

import (
	"context"
	"fmt"
	"os"

	"golang.org/x/term"

	"github.com/polius/fsend/internal/update"
	"github.com/polius/fsend/internal/version"
)

// printUpdateNotice prints a one-line "newer fsend available" hint after a
// successful transfer, when interactive. It's best-effort and silent on
// failure (see internal/update); the lookup is cached for a day so most
// runs do no network I/O.
//
// Skipped under --quiet (the stdout contract must stay clean) and when
// stderr isn't a terminal, so piped/scripted use never triggers the
// network call or the extra line.
func printUpdateNotice(f *flags) {
	if f.quiet || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	if msg := update.Notice(context.Background(), version.Version); msg != "" {
		fmt.Fprintf(os.Stderr, "\n  %s\n", msg)
	}
}
