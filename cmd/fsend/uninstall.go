package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/uxlog"
)

// runUninstall removes the fsend binary and config dir.
//
// Order: ask before doing anything. Then config dir, then binary.
// Config dir first because it's always writable; binary may live in a
// privileged directory and need the user to escalate by hand.
func runUninstall(f *flags) error {
	if !confirmUninstall(f) {
		fmt.Fprintln(os.Stderr, "  Uninstall cancelled.")
		return nil
	}

	cfgPath, _ := config.Path()
	if cfgPath != "" {
		dir := filepath.Dir(cfgPath)
		if err := os.RemoveAll(dir); err == nil {
			fmt.Fprintf(os.Stderr, "  %s Removed config dir: %s\n", uxlog.Check(), dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "  %s Could not remove %s: %v\n", uxlog.Warn(), dir, err)
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

	// On Windows the running .exe can't delete itself. Best we can do
	// is print the path and ask the user to remove it by hand.
	if runtime.GOOS == "windows" {
		fmt.Fprintf(os.Stderr, "  Delete this file to finish uninstall: %s\n", binPath)
		return nil
	}

	if err := os.Remove(binPath); err != nil {
		fmt.Fprintf(os.Stderr, "  %s Could not remove binary at %s: %v\n", uxlog.Warn(), binPath, err)
		fmt.Fprintln(os.Stderr, "    Remove it manually, possibly with sudo.")
		return nil
	}
	fmt.Fprintf(os.Stderr, "  %s Removed binary: %s\n", uxlog.Check(), binPath)

	// If the on-PATH name was a symlink, drop the dangling link too.
	if rawPath != binPath {
		if err := os.Remove(rawPath); err == nil {
			fmt.Fprintf(os.Stderr, "  %s Removed symlink: %s\n", uxlog.Check(), rawPath)
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
	fmt.Fprintln(os.Stderr, "This will remove:")
	if binPath != "" {
		fmt.Fprintf(os.Stderr, "  • binary:     %s\n", binPath)
	}
	if cfgDir != "" {
		fmt.Fprintf(os.Stderr, "  • config dir: %s\n", cfgDir)
	}
	fmt.Fprint(os.Stderr, "Continue? [y/N]: ")
	switch readLine(os.Stdin) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
