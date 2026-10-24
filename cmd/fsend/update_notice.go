package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/polius/fsend/internal/update"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
)

// updateCheck carries the background lookup's answer: the newer release,
// or "" when there is none (or the lookup failed or is still running).
var updateCheck struct {
	once sync.Once
	done chan struct{}
	tag  string
}

// startUpdateCheck runs the "newer fsend available" lookup in the
// background while the transfer runs, so it never delays the exit. Best-
// effort and silent on failure (see internal/update); cached for a day,
// so most runs do no network I/O at all.
//
// Skipped under --quiet (the stdout contract must stay clean) and when
// stderr isn't a terminal, so piped/scripted use never triggers the
// network call or the extra line.
func startUpdateCheck(f *flags) {
	if f.quiet || !term.IsTerminal(int(os.Stderr.Fd())) {
		return
	}
	updateCheck.once.Do(func() {
		updateCheck.done = make(chan struct{})
		go func() {
			defer close(updateCheck.done)
			if latest, ok := update.Available(context.Background(), version.Version); ok {
				updateCheck.tag = latest
			}
		}()
	})
}

// updateNoticeGrace is how long the summary waits for a lookup still in
// flight: enough for a warm cache or a fast network, never the 2 s timeout.
const updateNoticeGrace = 150 * time.Millisecond

// printUpdateNotice prints the hint after a successful transfer if the
// background lookup found a newer release; a lookup still running past
// updateNoticeGrace is dropped rather than waited on.
func printUpdateNotice(f *flags) {
	if f.quiet || updateCheck.done == nil {
		return
	}
	select {
	case <-updateCheck.done:
	case <-time.After(updateNoticeGrace):
		return
	}
	if updateCheck.tag != "" {
		fmt.Fprintf(os.Stderr, "\n%s\n", availableLine(updateCheck.tag, version.Version))
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
