package uxlog

import (
	"bytes"
	"os"
	"testing"
)

// silenceStderr redirects os.Stderr to /dev/null for the duration of the
// test. Progress writes through mpb to os.Stderr, and we don't want bar
// glyphs leaking into the test runner's output.
func silenceStderr(t *testing.T) {
	t.Helper()
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open /dev/null: %v", err)
	}
	orig := os.Stderr
	os.Stderr = devnull
	t.Cleanup(func() {
		os.Stderr = orig
		_ = devnull.Close()
	})
}

// TestProgress_KnownTotalLifecycle exercises the common case: a Progress
// with a known up-front total, partial Add() calls, and Done() at the
// end. The bar must accept the increments without panicking and the
// final Done() must return cleanly even when the bar reached 100 %.
func TestProgress_KnownTotalLifecycle(t *testing.T) {
	silenceStderr(t)
	p := New(1000)
	p.Add(400)
	p.Add(600)
	p.Done()
}

// TestProgress_AbortBeforeComplete covers the cancel-mid-transfer path:
// the bar is below 100 % when Done() runs. Done() must Abort the bar
// internally (verified by reaching the next line without hanging) rather
// than waiting forever for an Increment that will never come.
func TestProgress_AbortBeforeComplete(t *testing.T) {
	silenceStderr(t)
	p := New(1000)
	p.Add(250) // partial — bar at 25 %
	p.Done()   // must not hang
}

// TestProgress_StreamingSetTotal mirrors the streamed-stdin code path:
// the bar is constructed with total=0 (size unknown), bytes flow in via
// Add(), and at EOF the producer calls SetTotal(actual, true) so the
// bar latches to "done" instead of "aborted". Regression guard for the
// streaming-stdin work.
func TestProgress_StreamingSetTotal(t *testing.T) {
	silenceStderr(t)
	p := New(0)
	p.Add(123)
	p.Add(456)
	p.SetTotal(579, true)
	p.Done()
}

// TestProgress_NilSafety locks in the nil-receiver guards in Add /
// SetTotal / Done. Callers (the CLI's --quiet path) rely on being able
// to pass a nil *Progress without branching on it at every call site.
func TestProgress_NilSafety(t *testing.T) {
	var p *Progress
	p.Add(100)
	p.SetTotal(200, true)
	p.Done()
}

// TestProgress_PlainModeNoEscapes guards the non-TTY contract: piped
// progress output must contain no ANSI escapes (mpb's non-interactive
// mode emits cursor-up sequences, which is why plain mode bypasses it)
// and prints no completion line — the summary that follows carries the
// final size.
func TestProgress_PlainModeNoEscapes(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = orig; _ = f.Close() })

	p := New(1000)
	p.Add(400)
	p.Add(600)
	p.Done()

	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.ContainsRune(out, 0x1b) {
		t.Fatalf("plain progress emitted ANSI escapes: %q", out)
	}
	if got := string(out); got != "" {
		t.Fatalf("completed plain progress should print nothing: got %q", got)
	}
}

// TestProgress_PlainModePartialSilent: an aborted plain-mode transfer
// prints no terminal line — the error that follows is the record.
func TestProgress_PlainModePartialSilent(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = orig; _ = f.Close() })

	p := New(1000)
	p.Add(250)
	p.Done()

	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("partial plain progress should be silent, got %q", out)
	}
}
