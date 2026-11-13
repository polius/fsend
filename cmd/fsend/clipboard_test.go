package main

import (
	"errors"
	"runtime"
	"slices"
	"testing"
)

// The first helper that exists and exits 0 confirms the copy; missing or
// failing helpers fall through to the next, and none at all confirms
// nothing (OSC 52, unannounced, is the caller's fallback).
func TestCopyViaHelper(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	t.Setenv("WAYLAND_DISPLAY", "wayland-0")
	t.Setenv("DISPLAY", ":0")
	origRun, origLook := runClipboardHelper, lookPath
	t.Cleanup(func() { runClipboardHelper, lookPath = origRun, origLook })

	var got []string
	var ran []string
	runClipboardHelper = func(text string, argv []string) bool {
		got = append(got, text)
		ran = append(ran, argv[0])
		return argv[0] != "wl-copy" // the first helper fails, the next works
	}
	lookPath = func(name string) (string, error) { return "/bin/" + name, nil }
	if !copyViaHelper("fsend abc-defg-jkm") {
		t.Fatal("a working helper must confirm the copy")
	}
	if got[len(got)-1] != "fsend abc-defg-jkm" {
		t.Errorf("helper got %q", got)
	}
	if runtime.GOOS == "linux" && !slices.Equal(ran, []string{"wl-copy", "xclip"}) {
		t.Errorf("helpers tried = %v, want wl-copy then xclip", ran)
	}

	lookPath = func(string) (string, error) { return "", errors.New("not found") }
	if copyViaHelper("x") {
		t.Error("no helper on PATH must not confirm a copy")
	}
}

// Over SSH a local helper would fill the remote clipboard: none is tried.
func TestClipboardHelpersSkipSSH(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "10.0.0.1 22 10.0.0.2 22")
	if hs := clipboardHelpers(); len(hs) != 0 {
		t.Errorf("helpers over SSH = %v, want none", hs)
	}
}

// --password codes, --quiet, the opt-out and a non-terminal stderr never
// touch the clipboard.
func TestCopyCommandGuards(t *testing.T) {
	origRun := runClipboardHelper
	t.Cleanup(func() { runClipboardHelper = origRun })
	runClipboardHelper = func(string, []string) bool {
		t.Error("no helper may run")
		return true
	}
	for _, f := range []*flags{{passArg: "pw"}, {quiet: true}, {}} {
		if copyCommand(f, "fsend abc-defg-jkm") {
			t.Errorf("flags %+v copied", f)
		}
	}
	t.Setenv("FSEND_NO_CLIPBOARD", "1")
	if !clipboardDisabled() {
		t.Error("FSEND_NO_CLIPBOARD=1 must disable the copy")
	}
}

func TestOSC52(t *testing.T) {
	if got := osc52("fsend abc"); got != "\x1b]52;c;ZnNlbmQgYWJj\a" {
		t.Errorf("osc52 = %q", got)
	}
}
