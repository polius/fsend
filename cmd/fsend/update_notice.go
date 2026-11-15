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
		uxlog.Blank()
		printAvailable(updateCheck.tag, version.Version)
	}
}

// printAvailable prints the "a newer release exists" notice, shared by the
// passive notice and `--update --check` so the two can never disagree: a
// neutral line naming both versions, and under it what to do about it on
// this installation.
func printAvailable(latest, current string) {
	head, action := availableNotice(latest, current, installedBy())
	uxlog.Status(uxlog.Info(), head)
	uxlog.Line(action)
}

// availableNotice words the notice for an installation of the given kind.
// The second line must name something that works: `fsend --update` refuses
// (E039) on a binary a package manager owns, so brew gets brew's command
// and a deb, rpm or apk install is pointed at the release page, where the
// new package is — the packages come from the release, not a repository,
// so there is no one command to run. The command is accented (the thing to
// type); the link shows what changed before updating.
func availableNotice(latest, current string, by installer) (head, action string) {
	head = fmt.Sprintf("fsend %s is available (you have %s)", latest, strings.TrimPrefix(current, "v"))
	link := uxlog.Link(update.ReleaseURL(latest))
	switch {
	case by.pkg != "":
		return head, "Install the new ." + by.pkg + ": " + link
	case by.brew:
		return head, uxlog.Join(uxlog.Code("brew upgrade fsend"), link)
	}
	return head, uxlog.Join(uxlog.Code("fsend --update"), link)
}

// installer says what owns the running binary, which decides how it is
// updated: Homebrew, a Linux package (pkg is "deb", "rpm" or "apk"), or —
// the zero value — fsend itself (the install script, a hand-placed binary,
// go install), where `fsend --update` works.
type installer struct {
	brew bool
	pkg  string
}

// installedBy inspects the running binary the same way runUpdate's
// refusals do (managedByHomebrew, packagedAs), so the notice and the
// refusal agree. An unreadable path counts as fsend's own.
func installedBy() installer {
	exe, err := os.Executable()
	if err != nil {
		return installer{}
	}
	return installerOf(resolveBinaryPath(exe))
}

// installerOf is installedBy for a given binary path.
func installerOf(binPath string) installer {
	if managedByHomebrew(binPath) {
		return installer{brew: true}
	}
	return installer{pkg: packagedAs(binPath)}
}
