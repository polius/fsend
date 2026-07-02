package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/uxlog"
)

// runUninstall removes the fsend binary and config dir.
//
// Order: ask before doing anything. Then config dir, then binary.
// Config dir first because it's always writable; binary may live in a
// privileged directory and need the user to escalate by hand.
func runUninstall(f *flags) error {
	// A brew-managed binary belongs to brew: deleting it strands the
	// formula's metadata and `brew list/upgrade` keep believing it's
	// installed. Refuse before even asking for confirmation.
	if binPath, err := os.Executable(); err == nil {
		if resolved, rerr := filepath.EvalSymlinks(binPath); rerr == nil && resolved != "" {
			binPath = resolved
		}
		if managedByHomebrew(binPath) {
			return fmt.Errorf("%w: this fsend was installed with Homebrew — uninstall it with: brew uninstall fsend", fserrors.ErrUninstallFailed)
		}
	}

	if !confirmUninstall(f) {
		fmt.Fprintln(os.Stderr, "  Uninstall cancelled.")
		return nil
	}

	cfgPath, _ := config.Path()
	if cfgPath != "" {
		dir := filepath.Dir(cfgPath)
		if err := os.RemoveAll(dir); err == nil {
			fmt.Fprintf(os.Stderr, "%s Removed config dir: %s\n", uxlog.Check(), dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "%s Could not remove %s: %v\n", uxlog.Warn(), dir, err)
		}
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating fsend binary: %w", err)
	}
	// Resolve symlinks so we hit the actual binary, but also remember the
	// raw path. A `~/.local/bin/fsend → /opt/fsend/fsend` install needs
	// both removed; without the raw cleanup the on-PATH name survives as
	// a dangling link.
	resolved, _ := filepath.EvalSymlinks(binPath)
	rawPath := binPath
	if resolved != "" {
		binPath = resolved
	}

	// On Windows a running .exe can't delete its own image, so removeBinary
	// hands off to a detached helper that deletes it once we exit and strips
	// the install dir from PATH — keeping uninstall hands-off like elsewhere.
	if runtime.GOOS == "windows" {
		if err := removeBinary(binPath); err != nil {
			fmt.Fprintf(os.Stderr, "%s Could not remove binary at %s: %v\n", uxlog.Warn(), binPath, err)
			fmt.Fprintf(os.Stderr, "    Delete it manually to finish: %s\n", binPath)
			return fserrors.ErrUninstallFailed
		}
		fmt.Fprintf(os.Stderr, "%s Removed binary: %s\n", uxlog.Check(), binPath)
		fmt.Fprintln(os.Stderr, "  fsend uninstalled.")
		return nil
	}

	if err := os.Remove(binPath); err != nil {
		fmt.Fprintf(os.Stderr, "%s Could not remove binary at %s: %v\n", uxlog.Warn(), binPath, err)
		fmt.Fprintln(os.Stderr, "    Remove it manually, possibly with sudo.")
		return fserrors.ErrUninstallFailed
	}
	fmt.Fprintf(os.Stderr, "%s Removed binary: %s\n", uxlog.Check(), binPath)

	// If the on-PATH name was a symlink, drop the dangling link too.
	if rawPath != binPath {
		if err := os.Remove(rawPath); err == nil {
			fmt.Fprintf(os.Stderr, "%s Removed symlink: %s\n", uxlog.Check(), rawPath)
		}
	}

	fmt.Fprintln(os.Stderr, "  fsend uninstalled.")
	return nil
}

func confirmUninstall(f *flags) bool {
	if f.yes {
		return true
	}
	// Resolve concrete paths so the user can see what's about to go.
	binPath, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(binPath); err == nil && binPath != "" {
		binPath = resolved
	}
	cfgPath, _ := config.Path()
	cfgDir := ""
	if cfgPath != "" {
		cfgDir = filepath.Dir(cfgPath)
	}
	// Same shape as the receive prompts: two-space indent, no trailing
	// colon, ASCII list markers (the • glyph has no pipe fallback).
	fmt.Fprintln(os.Stderr, "  This will remove:")
	if binPath != "" {
		fmt.Fprintf(os.Stderr, "    - binary:     %s\n", binPath)
	}
	if cfgDir != "" {
		fmt.Fprintf(os.Stderr, "    - config dir: %s\n", cfgDir)
	}
	fmt.Fprint(os.Stderr, "  Continue? [y/N] ")
	line, _ := readLine(os.Stdin)
	switch line {
	case "y", "yes":
		return true
	default:
		return false
	}
}
