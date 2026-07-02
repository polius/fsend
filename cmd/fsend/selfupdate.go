package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/update"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
)

// runUpdate replaces the running binary with the latest release by
// re-running the platform installer (runInstaller, per-OS) pinned to
// the directory the binary lives in. The installer — not fsend — does
// the download and checksum verification, so the update path can never
// drift from a fresh install.
func runUpdate() error {
	current := strings.TrimPrefix(version.Version, "v")
	if current == "" || current == "dev" {
		return fmt.Errorf("%w: this is a dev build with no release to compare against", fserrors.ErrUpdateFailed)
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: locating the fsend binary: %v", fserrors.ErrUpdateFailed, err)
	}
	// Resolve symlinks so the install lands on the real binary and an
	// on-PATH `~/.local/bin/fsend → /opt/fsend/fsend` link keeps working.
	if resolved, rerr := filepath.EvalSymlinks(binPath); rerr == nil && resolved != "" {
		binPath = resolved
	}
	// A brew-managed binary must not be overwritten behind brew's back —
	// the Cellar file would diverge from the formula's metadata and the
	// next `brew upgrade` would fight it. Checked before any network work.
	if managedByHomebrew(binPath) {
		return fmt.Errorf("%w: this fsend was installed with Homebrew — update it with: brew upgrade fsend", fserrors.ErrUpdateFailed)
	}

	fmt.Fprintln(os.Stderr, "  Checking the latest release...")
	latest, ok := update.Latest(context.Background())
	if !ok {
		return fmt.Errorf("%w: could not look up the latest release", fserrors.ErrUpdateFailed)
	}
	if !update.Newer(latest, current) {
		fmt.Fprintf(os.Stderr, "%s fsend %s is already the latest version.\n", uxlog.Check(), current)
		return nil
	}

	fmt.Fprintf(os.Stderr, "  Updating fsend %s → %s in %s\n", current, latest, filepath.Dir(binPath))
	if err := runInstaller(binPath); err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrUpdateFailed, err)
	}
	return nil
}

// managedByHomebrew reports whether the (symlink-resolved) binary lives
// in a Homebrew prefix. Only brew is detected: its paths are unambiguous,
// and the project ships a brew tap so it's the real-world case — the
// curl script, go install, and hand-placed binaries stay self-managed.
func managedByHomebrew(binPath string) bool {
	for _, marker := range []string{"/Cellar/", "/homebrew/", "/linuxbrew/"} {
		if strings.Contains(binPath, marker) {
			return true
		}
	}
	return false
}
