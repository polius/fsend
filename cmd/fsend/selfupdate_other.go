//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
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
// directory binPath lives in, inheriting stdio so installer progress —
// and a possible sudo prompt — reach the user. Unix lets the installer
// rename over the running binary, so no move-aside dance is needed.
func runInstaller(binPath string) error {
	cmd := exec.Command("sh", "-c", installerScript)
	cmd.Env = append(os.Environ(), "PREFIX="+filepath.Dir(binPath))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
