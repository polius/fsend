//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// installerScript is the documented install one-liner, with a wget
// fallback mirroring the script's own download() support. The installer
// reads PREFIX from the environment.
const installerScript = `
if command -v curl >/dev/null 2>&1; then
    curl -fsSL https://getfsend.alzina.dev | sh
elif command -v wget >/dev/null 2>&1; then
    wget -qO- https://getfsend.alzina.dev | sh
else
    echo "need curl or wget to download the installer" >&2
    exit 1
fi
`

// runInstaller re-runs the install script pinned (via PREFIX) to the
// directory binPath lives in, inheriting stdio so installer progress
// reaches the user. Unix lets the installer rename over the running
// binary, so no move-aside dance is needed. The script is per-user and
// refuses root (runUpdate guards that before getting here).
func runInstaller(binPath string) error {
	cmd := exec.Command("sh", "-c", installerScript)
	// --update always wants the newest release: drop any FSEND_VERSION the
	// user exported, so a stale pin can't reinstall an older version over
	// a newer binary.
	env := slices.DeleteFunc(slices.Clone(os.Environ()), func(e string) bool {
		return strings.HasPrefix(e, "FSEND_VERSION=")
	})
	cmd.Env = append(env, "FSEND_VERSION=latest", "PREFIX="+filepath.Dir(binPath))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
