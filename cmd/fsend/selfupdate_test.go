package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// printAvailable is the single rendering shared by the passive notice and
// --update --check. Asserted uncoloured, which is what a pipe gets. The
// test binary is nobody's package, so the action is fsend's own command.
func TestPrintAvailable(t *testing.T) {
	got := captureStderr(t, func() { printAvailable("1.14.1", "1.14.0") })
	want := "[i] fsend 1.14.1 is available (you have 1.14.0)\n" +
		"  fsend --update · https://github.com/polius/fsend/releases/tag/v1.14.1\n"
	if got != want {
		t.Errorf("printAvailable =\n%q\nwant\n%q", got, want)
	}
}

// D2: the notice names an action that works on this installation.
// `fsend --update` refuses (E039) on a Homebrew or packaged binary, so the
// notice must never send those users to it.
func TestAvailableNotice_NamesTheRightAction(t *testing.T) {
	const link = "https://github.com/polius/fsend/releases/tag/v1.14.1"
	cases := []struct {
		name string
		by   installer
		want string
	}{
		{"install script", installer{}, "fsend --update · " + link},
		{"homebrew", installer{brew: true}, "brew upgrade fsend · " + link},
		{"deb", installer{pkg: "deb"}, "Install the new .deb: " + link},
		{"rpm", installer{pkg: "rpm"}, "Install the new .rpm: " + link},
		{"apk", installer{pkg: "apk"}, "Install the new .apk: " + link},
	}
	for _, c := range cases {
		head, action := availableNotice("1.14.1", "v1.14.0", c.by)
		if head != "fsend 1.14.1 is available (you have 1.14.0)" {
			t.Errorf("%s: head = %q", c.name, head)
		}
		if action != c.want {
			t.Errorf("%s: action = %q, want %q", c.name, action, c.want)
		}
		if selfManaged := c.by == (installer{}); !selfManaged && strings.Contains(action, "fsend --update") {
			t.Errorf("%s: the notice points at fsend --update, which refuses here: %q", c.name, action)
		}
		// The line fits 80 columns under its two-column indent.
		if n := len("  " + action); n > 79 {
			t.Errorf("%s: action line is %d columns: %q", c.name, n, action)
		}
	}
}

// installerOf reads the same evidence runUpdate's refusals do: the notice
// and the refusal can't disagree about who owns the binary.
func TestInstallerOf(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "usr", "bin", "fsend")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := installerOf(bin); got != (installer{}) {
		t.Errorf("no marker: installerOf = %+v, want fsend's own", got)
	}
	marker := filepath.Join(root, "usr", "share", "fsend", "package-manager")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"deb", "rpm", "apk"} {
		if err := os.WriteFile(marker, []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := installerOf(bin); got != (installer{pkg: f}) {
			t.Errorf("%s marker: installerOf = %+v", f, got)
		}
	}
	for _, brewBin := range []string{
		"/opt/homebrew/Cellar/fsend/1.2.3/bin/fsend",
		"/home/linuxbrew/.linuxbrew/Cellar/fsend/1.2.3/bin/fsend",
	} {
		if got := installerOf(brewBin); got != (installer{brew: true}) {
			t.Errorf("installerOf(%q) = %+v, want Homebrew", brewBin, got)
		}
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

// The installer must be pinned to the version fsend verified and to the
// binary's own directory, whatever the user exported.
func TestInstallerEnv(t *testing.T) {
	t.Setenv("FSEND_VERSION", "0.0.1")
	t.Setenv("FSEND_PREFIX", "/elsewhere")
	t.Setenv("PREFIX", "/elsewhere")
	bin := filepath.Join(t.TempDir(), "fsend")
	got := map[string][]string{}
	for _, e := range installerEnv(bin, "1.2.3") {
		k, v, _ := strings.Cut(e, "=")
		got[k] = append(got[k], v)
	}
	for k, want := range map[string]string{
		"FSEND_VERSION": "v1.2.3",
		"FSEND_PREFIX":  filepath.Dir(bin),
		"FSEND_UPDATE":  "1",
	} {
		if len(got[k]) != 1 || got[k][0] != want {
			t.Errorf("%s = %q, want exactly [%q]", k, got[k], want)
		}
	}
	if len(got["PREFIX"]) != 0 {
		t.Errorf("PREFIX = %q, want it dropped", got["PREFIX"])
	}
}

// A binary installed from the deb/rpm/apk package carries a marker next to
// it; --update and --uninstall leave it to the package manager.
func TestPackagedAs(t *testing.T) {
	root := t.TempDir()
	bin := filepath.Join(root, "usr", "bin", "fsend")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := packagedAs(bin); got != "" {
		t.Fatalf("no marker: packagedAs = %q", got)
	}
	marker := filepath.Join(root, "usr", "share", "fsend", "package-manager")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"deb", "rpm", "apk"} {
		if err := os.WriteFile(marker, []byte(f+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := packagedAs(bin); got != f {
			t.Errorf("packagedAs = %q, want %q", got, f)
		}
		if !strings.Contains(packageAdvice(f, false), "releases/latest") || !strings.Contains(packageAdvice(f, true), "fsend") {
			t.Errorf("%s advice: %q / %q", f, packageAdvice(f, false), packageAdvice(f, true))
		}
	}
	if err := os.WriteFile(marker, []byte("snap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := packagedAs(bin); got != "" {
		t.Errorf("unknown format: packagedAs = %q", got)
	}
}
