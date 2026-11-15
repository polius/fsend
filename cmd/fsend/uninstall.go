package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

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
		if format := packagedAs(binPath); format != "" {
			return fmt.Errorf("%w: %s", fserrors.ErrHomebrewManaged, packageAdvice(format, true))
		}
	}

	if err := confirmUninstall(f); err != nil {
		// "n" is an answer, not a failure: exit 0.
		if errors.Is(err, errDeclined) {
			uxlog.Status(uxlog.Info(), "Not uninstalled.")
			return nil
		}
		return err
	}

	cfgPath, _ := config.Path()
	if cfgPath != "" {
		dir := filepath.Dir(cfgPath)
		if err := os.RemoveAll(dir); err == nil {
			uxlog.Status(uxlog.Check(), "Removed config dir: "+dir)
		} else if !errors.Is(err, os.ErrNotExist) {
			uxlog.Status(uxlog.Warn(), fmt.Sprintf("Could not remove %s: %v", dir, err))
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
		keptPath, err := removeBinary(binPath)
		if err != nil {
			uxlog.Status(uxlog.Warn(), fmt.Sprintf("Could not remove binary at %s: %v", binPath, err))
			uxlog.Line("Delete it manually to finish: " + binPath)
			return fserrors.ErrUninstallFailed
		}
		uxlog.Status(uxlog.Check(), "Removed binary: "+binPath)
		uxlog.Line("fsend uninstalled.")
		if keptPath {
			uxlog.Line(fmt.Sprintf("Left %s on your PATH: other files live there.", filepath.Dir(binPath)))
		}
		return nil
	}

	if err := os.Remove(binPath); err != nil {
		uxlog.Status(uxlog.Warn(), fmt.Sprintf("Could not remove binary at %s: %v", binPath, err))
		uxlog.Line("Remove it manually, possibly with sudo.")
		return fserrors.ErrUninstallFailed
	}
	uxlog.Status(uxlog.Check(), "Removed binary: "+binPath)

	// If the on-PATH name was a symlink, drop the dangling link too.
	if rawPath != binPath {
		if err := os.Remove(rawPath); err == nil {
			uxlog.Status(uxlog.Check(), "Removed symlink: "+rawPath)
		}
	}

	uxlog.Line("fsend uninstalled.")
	// Shell rc files are out of reach here (unlike Windows, where the
	// helper strips the PATH entry itself), so point at the exact entry.
	uxlog.Line(fmt.Sprintf("If your shell rc has a PATH entry for %s, remove it.", filepath.Dir(rawPath)))
	uxlog.Line("Also remove any \"fsend completion\" line you added.")
	return nil
}

// onlyFsendIn reports whether dir holds nothing but the fsend binary at
// binPath (and the .old copy an interrupted Windows update can leave).
// Uninstall drops the dir from PATH only then: a shared dir like
// ~/bin keeps its entry for the other tools in it. An unreadable dir
// counts as shared, the safe answer.
func onlyFsendIn(dir, binPath string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	name := filepath.Base(binPath)
	for _, e := range entries {
		if !strings.EqualFold(e.Name(), name) && !strings.EqualFold(e.Name(), name+".old") {
			return false
		}
	}
	return true
}

// errDeclined is the user answering "n" at a confirmation prompt.
var errDeclined = errors.New("declined at the prompt")

// confirmUninstall asks before anything is removed. An explicit "n"
// returns errDeclined (a choice, exit 0); EOF and Ctrl-C return
// ErrUserCancelled, the E026 / exit 130 of every other cancel path.
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
	// Same shape as the receive prompts: the list on the grid with ASCII
	// markers (the • glyph has no pipe fallback), then the question behind
	// the ? gutter.
	uxlog.Line("This will remove:")
	if binPath != "" {
		uxlog.Item("- binary:     " + binPath)
	}
	if cfgDir != "" {
		uxlog.Item("- config dir: " + cfgDir)
	}
	for {
		uxlog.Question("Continue?", "[y/N]")
		line, eof, ok := readLineCtx(ctx)
		if !ok {
			return fserrors.ErrUserCancelled
		}
		if eof {
			uxlog.Blank()
			uxlog.Status(uxlog.Info(), "No input to answer the prompt — not uninstalling.")
			return fserrors.ErrUserCancelled
		}
		switch line {
		case "y", "yes":
			return nil
		case "", "n", "no":
			return errDeclined
		}
		uxlog.Line("Please answer y or n.")
	}
}
