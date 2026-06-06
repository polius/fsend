package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/polius/fsend/internal/config"
)

// runUninstall removes the fsend binary and config dir.
//
// Order: ask before doing anything. Then config dir, then binary.
// Config dir first because it's always writable; binary may live in a
// privileged directory and need the user to escalate by hand.
func runUninstall(f *flags) error {
	if !confirmUninstall(f) {
		fmt.Fprintln(os.Stderr, "  uninstall cancelled")
		return nil
	}

	cfgPath, _ := config.Path()
	if cfgPath != "" {
		dir := filepath.Dir(cfgPath)
		if err := os.RemoveAll(dir); err == nil {
			fmt.Fprintf(os.Stderr, "  removed config dir: %s\n", dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			fmt.Fprintf(os.Stderr, "  warning: could not remove %s: %v\n", dir, err)
		}
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating fsend binary: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(binPath)
	if err == nil {
		binPath = resolved
	}

	// On Windows the running .exe can't delete itself. Best we can do
	// is print the path and ask the user to remove it by hand.
	if runtime.GOOS == "windows" {
		fmt.Fprintf(os.Stderr, "  delete this file to finish uninstall: %s\n", binPath)
		return nil
	}

	if err := os.Remove(binPath); err != nil {
		fmt.Fprintf(os.Stderr, "  could not remove binary at %s: %v\n", binPath, err)
		fmt.Fprintln(os.Stderr, "    Remove it manually, possibly with sudo.")
		return nil
	}
	fmt.Fprintf(os.Stderr, "  removed binary: %s\n", binPath)
	fmt.Fprintln(os.Stderr, "  fsend uninstalled.")
	return nil
}

func confirmUninstall(f *flags) bool {
	if f.yes {
		return true
	}
	fmt.Fprintln(os.Stderr, "This will remove the fsend binary and ~/.config/fsend.")
	fmt.Fprint(os.Stderr, "Continue? [y/N]: ")
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}
