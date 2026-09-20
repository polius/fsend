package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/version"
)

// Dev builds have no release version to compare against, so --update
// must refuse up front — before any output or network I/O.
func TestRunUpdate_DevBuildRefused(t *testing.T) {
	orig := version.Version
	version.Version = "dev"
	t.Cleanup(func() { version.Version = orig })

	err := runUpdate()
	if !errors.Is(err, fserrors.ErrUpdateFailed) {
		t.Fatalf("got %v, want wrapping ErrUpdateFailed", err)
	}
	if !strings.Contains(err.Error(), "dev build") {
		t.Errorf("got %q, want a dev-build explanation", err.Error())
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
