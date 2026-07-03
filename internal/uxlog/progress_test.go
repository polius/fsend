package uxlog

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"
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
	p := New(1000, false)
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
	p := New(1000, false)
	p.Add(250) // partial — bar at 25 %
	p.Done()   // must not hang
}

// TestProgress_StreamingSetTotal mirrors the streamed-stdin code path:
// the bar is constructed with total=0 (size unknown), bytes flow in via
// Add(), and at EOF the producer calls SetTotal(actual, true) so the
// bar completes (and is removed) instead of aborting. Regression guard
// for the streaming-stdin work.
func TestProgress_StreamingSetTotal(t *testing.T) {
	silenceStderr(t)
	p := New(0, false)
	p.Add(123)
	p.Add(456)
	p.SetTotal(579, true)
	p.Done()
}

// TestProgress_NilSafety locks in the nil-receiver guards in Add /
// SetTotal / SetLabel / Done. Callers (the CLI's --quiet path) rely on
// being able to pass a nil *Progress without branching on it at every
// call site.
func TestProgress_NilSafety(t *testing.T) {
	var p *Progress
	p.Add(100)
	p.SetTotal(200, true)
	p.SetLabel("file.txt")
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

	p := New(1000, false)
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

	p := New(1000, false)
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

// An unknown-total (stdin) stream must show throughput once past
// HumanRate's noise floor — a bare byte counter is all a long pipe would
// otherwise ever display.
func TestProgress_PlainModeUnknownTotalShowsRate(t *testing.T) {
	var buf bytes.Buffer
	p := &plainProgress{w: &buf, start: time.Now().Add(-2 * time.Second), lastLine: time.Now().Add(-2 * time.Second)}
	p.add(5 * 1000 * 1000) // 5 MB over ~2s → ~2.5 MB/s
	out := buf.String()
	if !strings.Contains(out, "5 MB") {
		t.Errorf("missing byte counter: %q", out)
	}
	if !strings.Contains(out, "/s") {
		t.Errorf("unknown-total line missing rate: %q", out)
	}

	// Below the noise floor: counter only, no misleading rate.
	buf.Reset()
	p2 := &plainProgress{w: &buf, start: time.Now().Add(-2 * time.Second), lastLine: time.Now().Add(-2 * time.Second)}
	p2.add(10 * 1000)
	if out := buf.String(); strings.Contains(out, "/s") {
		t.Errorf("sub-floor stream must not show a rate: %q", out)
	}
}

// Plain mode carries the current-file chip in its throttled line — still
// one line per print and zero ANSI (the caller sanitizes the name).
func TestProgress_PlainModeLabelInLine(t *testing.T) {
	var buf bytes.Buffer
	p := &plainProgress{w: &buf, total: 1000, start: time.Now().Add(-2 * time.Second), lastLine: time.Now().Add(-2 * time.Second)}
	p.setLabel("docs/report.pdf")
	p.add(400)
	out := buf.String()
	if !strings.Contains(out, "  ·  docs/report.pdf") {
		t.Errorf("plain line missing current-file chip: %q", out)
	}
	if bytes.ContainsRune(buf.Bytes(), 0x1b) {
		t.Errorf("plain line with label emitted ANSI escapes: %q", out)
	}
	if strings.Count(out, "\n") != 1 {
		t.Errorf("plain progress must print one line per update: %q", out)
	}
}

// A caller whose accounting overshoots the total must never see a
// percentage past 100 — the raw counters still expose the mismatch.
func TestProgress_PlainModePercentClamped(t *testing.T) {
	var buf bytes.Buffer
	p := &plainProgress{w: &buf, total: 1000, start: time.Now().Add(-2 * time.Second), lastLine: time.Now().Add(-2 * time.Second)}
	p.add(1180)
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "100%") {
		t.Errorf("overshot progress must clamp to 100%%: %q", out)
	}
}

// truncateName keeps the tail (extension) visible with a middle ellipsis
// and leaves short names untouched.
func TestTruncateName(t *testing.T) {
	if got := truncateName("short.txt", 20); got != "short.txt" {
		t.Errorf("short name changed: %q", got)
	}
	got := truncateName("a-very-long-directory/deeply/nested/file.tar.gz", 20)
	if r := []rune(got); len(r) != 20 {
		t.Errorf("truncated to %d runes, want 20: %q", len(r), got)
	}
	if !strings.HasSuffix(got, ".tar.gz") || !strings.Contains(got, "…") {
		t.Errorf("truncation must keep the tail and show an ellipsis: %q", got)
	}
}
