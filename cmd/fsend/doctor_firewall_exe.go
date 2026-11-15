//go:build darwin || windows

package main

import (
	"os"
	"path/filepath"
)

// exePaths lists the paths this fsend may be known to an application
// firewall under: the one it was started as and, when that is a link (a
// Homebrew or winget shim), the file it resolves to.
func exePaths() []string {
	exe, err := os.Executable()
	if err != nil {
		return nil
	}
	paths := []string{exe}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil && resolved != exe {
		paths = append(paths, resolved)
	}
	return paths
}
