//go:build !windows

package main

import (
	"os"
	"os/exec"
)

// installerAsset is the release asset runUpdate verifies and runs.
const installerAsset = "install.sh"

// runInstaller runs the verified install script pinned to version and to
// the directory binPath lives in (installerEnv), inheriting stdio so
// installer progress reaches the user. Unix lets the installer rename
// over the running binary, so no move-aside dance is needed. The script
// is per-user and refuses root (runUpdate guards that before getting here).
func runInstaller(binPath, script, version string) error {
	cmd := exec.Command("sh", script) //nolint:gosec // G204: the installer FetchInstaller just verified
	cmd.Env = installerEnv(binPath, version)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
