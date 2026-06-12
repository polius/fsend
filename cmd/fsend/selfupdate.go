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

	fmt.Fprintln(os.Stderr, "  Checking the latest release...")
	latest, ok := update.Latest(context.Background())
	if !ok {
		return fmt.Errorf("%w: could not look up the latest release", fserrors.ErrUpdateFailed)
	}
	if !update.Newer(latest, current) {
		fmt.Fprintf(os.Stderr, "%s fsend %s is already the latest version.\n", uxlog.Check(), current)
		return nil
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

	fmt.Fprintf(os.Stderr, "  Updating fsend %s → %s in %s\n", current, latest, filepath.Dir(binPath))
	if err := runInstaller(binPath); err != nil {
		return fmt.Errorf("%w: %v", fserrors.ErrUpdateFailed, err)
	}
	return nil
}
