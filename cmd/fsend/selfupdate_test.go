package main

import (
	"errors"
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
