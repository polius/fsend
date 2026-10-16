package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
//
// check reports instead of installing: it shares the lookup and the
// wording, and stops before anything is written.
func runUpdate(check bool) error {
	current := strings.TrimPrefix(version.Version, "v")
	if current == "" || current == "dev" {
		return fserrors.ErrUpdateDevBuild
	}

	binPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("%w: locating the fsend binary: %v", fserrors.ErrUpdateFailed, err)
	}
	// Resolve symlinks — but only when the binary itself is one — so the
	// install lands on the real binary and an on-PATH
	// `~/.local/bin/fsend → /opt/fsend/fsend` link keeps working. Always
	// resolving would also rewrite symlinked directories (/var →
	// /private/var on macOS), handing the installer a physical prefix
	// that PATH spells differently: the installer then appends a
	// duplicate PATH line and warns about a shadow that isn't there.
	binPath = resolveBinaryPath(binPath)

	// The two refusals below exist to protect a write, so --check — which
	// only looks — is exempt from both.
	if !check {
		// A brew-managed binary must not be overwritten behind brew's back —
		// the Cellar file would diverge from the formula's metadata and the
		// next `brew upgrade` would fight it. Checked before any network work.
		if managedByHomebrew(binPath) {
			return fmt.Errorf("%w: update it with: brew upgrade fsend", fserrors.ErrHomebrewManaged)
		}

		// The installers are per-user and refuse root (see scripts/install.*).
		// Mirror that here so the failure happens before any network work, with
		// an actionable message instead of an installer abort mid-update. The
		// same FSEND_ALLOW_ROOT opt-in applies: a root install (container,
		// appliance) must not become a dead end for --update. The env var
		// carries through to the installer re-run below, so it refuses only
		// once. Geteuid returns -1 on Windows, where the check does not apply.
		if os.Geteuid() == 0 && !rootOptIn() {
			return fserrors.ErrUpdateRootRefused
		}
	}

	fmt.Fprintln(os.Stderr, uxlog.Step(), "Checking the latest release...")
	latest, ok := update.Latest(context.Background())
	if !ok {
		return fmt.Errorf("%w: could not look up the latest release", fserrors.ErrUpdateFailed)
	}
	if !update.Newer(latest, current) {
		fmt.Fprintf(os.Stderr, "%s fsend %s is already the latest version.\n", uxlog.Check(), current)
		return nil
	}
	if check {
		fmt.Fprintf(os.Stderr, "%s\n", availableLine(latest, current))
		return nil
	}

	fmt.Fprintf(os.Stderr, "%s Updating fsend %s → %s in %s\n", uxlog.Step(), current, latest, filepath.Dir(binPath))
	fmt.Fprintf(os.Stderr, "  what's new: %s\n", uxlog.Link(update.ReleaseURL(latest)))
	if err := runInstaller(binPath); err != nil {
		return fmt.Errorf("%w: %s", fserrors.ErrUpdateFailed, installerFailureDetail(err))
	}
	return nil
}

// installerFailureDetail phrases a failed installer run for a person:
// exec's bare "exit status 1" becomes "the installer exited with status
// 1". Anything else (no shell on PATH, say) passes through as-is.
func installerFailureDetail(err error) string {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("the installer exited with status %d", exit.ExitCode())
	}
	return err.Error()
}

// rootOptIn reports whether the user explicitly accepted a root install
// or update via FSEND_ALLOW_ROOT — the escape hatch for single-user
// machines (containers, appliances) that have no other user. Same value
// semantics as FORCE_COLOR: empty, "0", or "false" means off.
func rootOptIn() bool {
	switch os.Getenv("FSEND_ALLOW_ROOT") {
	case "", "0", "false":
		return false
	}
	return true
}

// resolveBinaryPath resolves the executable's path through file
// symlinks, leaving it alone otherwise. See the call site in runUpdate
// for why directory symlinks must stay untouched.
func resolveBinaryPath(binPath string) string {
	if fi, err := os.Lstat(binPath); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if resolved, rerr := filepath.EvalSymlinks(binPath); rerr == nil && resolved != "" {
			return resolved
		}
	}
	return binPath
}

// managedByHomebrew reports whether the (symlink-resolved) binary lives
// in a Homebrew prefix. Only brew is detected: its paths are unambiguous,
// and the project ships a brew tap so it's the real-world case — the
// curl script, go install, and hand-placed binaries stay self-managed.
func managedByHomebrew(binPath string) bool {
	for _, marker := range []string{"/Cellar/", "/Caskroom/", "/homebrew/", "/linuxbrew/"} {
		if strings.Contains(binPath, marker) {
			return true
		}
	}
	return false
}
