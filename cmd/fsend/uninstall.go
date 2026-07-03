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
			return fmt.Errorf("%w: uninstall it with: brew uninstall fsend", fserrors.ErrHomebrewManaged)
		}
	}

	if err := confirmUninstall(f); err != nil {
		return err
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
	// Shell rc files are out of reach here (unlike Windows, where the
	// helper strips the PATH entry itself), so leave it to the user.
	fmt.Fprintln(os.Stderr, "  If you added a PATH export or \"fsend completion\" line to your shell rc, remove it.")
	return nil
}

// confirmUninstall asks before anything is removed. Decline, EOF, and
// Ctrl-C all return ErrUserCancelled so scripts see the same E026 / exit
// 130 as every other cancel path.
func confirmUninstall(f *flags) error {
	if f.yes {
		return nil
	}
	ctx, cancel := signalContext(f.quiet)
	defer cancel()
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
	for {
		fmt.Fprint(os.Stderr, "  Continue? [y/N] ")
		line, eof, ok := readLineCtx(ctx)
		if !ok {
			return fserrors.ErrUserCancelled
		}
		if eof {
			fmt.Fprintf(os.Stderr, "\n%s No input to answer the prompt — not uninstalling.\n", uxlog.Info())
			return fserrors.ErrUserCancelled
		}
		switch line {
		case "y", "yes":
			return nil
		case "", "n", "no":
			return fserrors.ErrUserCancelled
		}
		fmt.Fprintln(os.Stderr, "  Please answer y or n.")
	}
}
