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
