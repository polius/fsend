package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/version"
)

// Dev builds have no release version to compare against, so --update
// must refuse up front — before any output or network I/O — with its own
// code, not E033's "check your internet connection".
func TestRunUpdate_DevBuildRefused(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = orig })

	err := runUpdate(false)
	if !errors.Is(err, fserrors.ErrUpdateDevBuild) {
		t.Fatalf("got %v, want wrapping ErrUpdateDevBuild", err)
	}
	// Same refusal for the report-only path: --check can't report on a
	// build that has no release to compare with either.
	if err := runUpdate(true); !errors.Is(err, fserrors.ErrUpdateDevBuild) {
		t.Fatalf("--check got %v, want wrapping ErrUpdateDevBuild", err)
	}
}

// The installer's exit is phrased for a person; other failures (no shell
// on PATH) pass through untouched.
func TestInstallerFailureDetail(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 7")
	err := cmd.Run()
	if got := installerFailureDetail(err); got != "the installer exited with status 7" {
		t.Errorf("exit failure: got %q", got)
	}
	if got := installerFailureDetail(errors.New("exec: \"sh\": executable file not found in $PATH")); got != `exec: "sh": executable file not found in $PATH` {
		t.Errorf("non-exit failure: got %q, want it verbatim", got)
	}
}

// availableLine is the single rendering shared by the passive notice and
// --update --check. Asserted uncoloured, which is what a pipe gets.
func TestAvailableLine(t *testing.T) {
	got := availableLine("1.14.1", "1.14.0")
	want := "[i] fsend 1.14.1 is available (you have 1.14.0)\n" +
		"  fsend --update  ·  https://github.com/polius/fsend/releases/tag/v1.14.1"
	if got != want {
		t.Errorf("availableLine =\n%q\nwant\n%q", got, want)
	}
}

// The path handed to the installer must keep directory symlinks as
// PATH spells them (a /var → /private/var rewrite would make the
// installer append a duplicate PATH line and warn about a shadow that
// isn't there), but must still resolve a symlinked binary so an
// on-PATH `~/.local/bin/fsend → /opt/fsend/fsend` link keeps working.
func TestResolveBinaryPath(t *testing.T) {
	realDir := t.TempDir()
	realBin := filepath.Join(realDir, "fsend")
	if err := os.WriteFile(realBin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Regular binary inside a symlinked directory: unchanged.
	linkDir := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	if got := resolveBinaryPath(filepath.Join(linkDir, "fsend")); got != filepath.Join(linkDir, "fsend") {
		t.Errorf("regular file in symlinked dir: got %q, want the logical path unchanged", got)
	}

	// Symlinked binary: resolved to its target. Compare against the
	// target's own resolved form — on macOS t.TempDir() hands out a
	// logical /var/... path whose physical spelling is /private/var/...,
	// which is precisely the rewrite this fix keeps out of PREFIX.
	linkBin := filepath.Join(t.TempDir(), "fsend")
	if err := os.Symlink(realBin, linkBin); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(realBin)
	if err != nil {
		t.Fatal(err)
	}
	if got := resolveBinaryPath(linkBin); got != want {
		t.Errorf("symlinked file: got %q, want %q", got, want)
	}

	// Plain path: unchanged.
	if got := resolveBinaryPath(realBin); got != realBin {
		t.Errorf("plain path: got %q, want %q", got, realBin)
	}
}

func TestRootOptIn(t *testing.T) {
	for _, off := range []string{"", "0", "false"} {
		t.Setenv("FSEND_ALLOW_ROOT", off)
		if rootOptIn() {
			t.Errorf("rootOptIn with FSEND_ALLOW_ROOT=%q = true, want false", off)
		}
	}
	for _, on := range []string{"1", "true", "yes"} {
		t.Setenv("FSEND_ALLOW_ROOT", on)
		if !rootOptIn() {
			t.Errorf("rootOptIn with FSEND_ALLOW_ROOT=%q = false, want true", on)
		}
	}
}

func TestManagedByHomebrew(t *testing.T) {
	for _, managed := range []string{
		"/opt/homebrew/Cellar/fsend/1.9.1/bin/fsend",
		"/usr/local/Cellar/fsend/1.9.1/bin/fsend",
		"/usr/local/Caskroom/fsend/1.9.1/fsend", // cask on Intel: no Cellar, no /homebrew/
		"/home/linuxbrew/.linuxbrew/bin/fsend",
	} {
		if !managedByHomebrew(managed) {
			t.Errorf("managedByHomebrew(%q) = false, want true", managed)
		}
	}
	for _, own := range []string{
		"/usr/local/bin/fsend", // curl-script default
		"/home/u/go/bin/fsend", // go install
		"/opt/fsend/fsend",
	} {
		if managedByHomebrew(own) {
			t.Errorf("managedByHomebrew(%q) = true, want false", own)
		}
	}
}
