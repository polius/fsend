package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/polius/fsend/internal/update"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
)

// printUpdateNotice prints a "newer fsend available" hint after a
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
	if latest, ok := update.Available(context.Background(), version.Version); ok {
		fmt.Fprintf(os.Stderr, "\n%s\n", availableLine(latest, version.Version))
	}
}

// availableLine is the "a newer release exists" hint, shared by the
// passive notice and `--update --check` so the two can never disagree.
// It leads with the info glyph, accents the command (the thing to type)
// and links the release, so the user can see what changed before
// updating.
func availableLine(latest, current string) string {
	return fmt.Sprintf("%s fsend %s is available (you have %s)\n  %s  ·  %s",
		uxlog.Info(), latest, strings.TrimPrefix(current, "v"),
		uxlog.Code(updateCommand()), uxlog.Link(update.ReleaseURL(latest)))
}

// updateCommand is how this installation is updated: brew's command when
// brew owns the binary — `fsend --update` there would only refuse with
// E039 — and fsend's own otherwise.
func updateCommand() string {
	if exe, err := os.Executable(); err == nil && managedByHomebrew(resolveBinaryPath(exe)) {
		return "brew upgrade fsend"
	}
	return "fsend --update"
}
