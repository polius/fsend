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
	p := New(1000, false, "LAN")
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
	p := New(1000, false, "LAN")
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
	p := New(0, false, "relay")
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

	p := New(1000, false, "LAN")
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

	p := New(1000, false, "LAN")
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

// Plain mode carries the connection-route chip the same way — one line
// per print, zero ANSI.
func TestProgress_PlainModeRouteInLine(t *testing.T) {
	var buf bytes.Buffer
	p := &plainProgress{w: &buf, total: 1000, start: time.Now().Add(-2 * time.Second), lastLine: time.Now().Add(-2 * time.Second), route: "relay"}
	p.add(400)
	out := buf.String()
	if !strings.Contains(out, "  ·  relay") {
		t.Errorf("plain line missing route chip: %q", out)
	}
	if bytes.ContainsRune(buf.Bytes(), 0x1b) {
		t.Errorf("plain line with route emitted ANSI escapes: %q", out)
	}
}

// CI logs get the rate and ETA too: nobody watches a bar there.
func TestProgress_PlainModeRateAndETA(t *testing.T) {
	var buf bytes.Buffer
	past := time.Now().Add(-2 * time.Second)
	p := &plainProgress{w: &buf, total: 100_000_000, start: past, lastLine: past}
	p.win.observe(past, 0)
	p.add(50_000_000)
	out := buf.String()
	if !strings.Contains(out, "MB/s") || !strings.Contains(out, "ETA ") {
		t.Errorf("plain line missing rate/ETA: %q", out)
	}
}

// etaLabel rounds projections up to whole seconds — never milliseconds —
// so the bar's tail doesn't flicker "ETA 943ms" at 10 Hz.
func TestETALabel(t *testing.T) {
	cases := []struct {
		secs float64
		want string
	}{
		{0.042, "1s"}, // sub-second rounds up, never "42ms"
		{0.943, "1s"},
		{1.2, "2s"},
		{9.01, "10s"},
		{59.5, "1m00s"},
		{90, "1m30s"},
	}
	for _, c := range cases {
		if got := etaLabel(c.secs); got != c.want {
			t.Errorf("etaLabel(%v) = %q, want %q", c.secs, got, c.want)
		}
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

// The plain fallback carries the same "· file i/N" counter the TTY chip
// does, so a folder of many small files reads as finite in CI logs too.
func TestProgress_PlainModeFileCounter(t *testing.T) {
	var buf bytes.Buffer
	p := &plainProgress{w: &buf, total: 1000, start: time.Now().Add(-2 * time.Second), lastLine: time.Now().Add(-2 * time.Second)}
	p.setLabel("data.bin")
	p.setFilePos(1, 3)
	p.add(500)
	if out := buf.String(); !strings.Contains(out, "data.bin  ·  file 1/3") {
		t.Errorf("plain line missing label + counter: %q", out)
	}
}

// A negative running total (a caller under-accounting) must clamp to 0%, not
// print a "-N%" line.
func TestProgress_PlainModePercentClampedLow(t *testing.T) {
	var buf bytes.Buffer
	p := &plainProgress{w: &buf, total: 1000, start: time.Now().Add(-2 * time.Second), lastLine: time.Now().Add(-2 * time.Second)}
	p.add(-50)
	out := buf.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "0%") {
		t.Errorf("negative progress must clamp to 0%%: %q", out)
	}
}

// Priority is % > ETA > rate > counter > name > bar > route: a narrow row
// drops whole elements from the low end instead of letting mpb cut a number
// mid-digit, and spare columns grow the bar, then the name.
func TestLayout(t *testing.T) {
	const big = 100_000_000 // 100 MB: rate/ETA chips ride along
	cases := []struct {
		name      string
		width     int
		total     int64
		showNames bool
		route     string
		want      barLayout
	}{
		{"40 cols keeps %, ETA and rate", 40, big, true, "LAN",
			barLayout{eta: true, rate: true}},
		{"60 cols adds the counter, no track", 60, big, true, "LAN",
			barLayout{eta: true, rate: true, counter: true}},
		{"80 cols adds the name before the bar", 80, big, true, "LAN",
			barLayout{eta: true, rate: true, counter: true, nameCap: 16}},
		{"100 cols: bar and name share the spare", 100, big, true, "LAN",
			barLayout{bar: 20, eta: true, rate: true, counter: true, nameCap: 14}},
		{"140 cols books the route", 140, big, true, "LAN",
			barLayout{bar: 36, eta: true, rate: true, counter: true, nameCap: 30, route: true}},
		{"200 cols: bar capped, name takes the rest", 200, big, true, "LAN",
			barLayout{bar: 40, eta: true, rate: true, counter: true, nameCap: 86, route: true}},
		{"no names means no chip", 200, big, false, "LAN",
			barLayout{bar: 40, eta: true, rate: true, counter: true, route: true}},
		{"no rate/ETA below the threshold", 100, 900_000, true, "LAN",
			barLayout{bar: 28, counter: true, nameCap: 30, route: true}},
		{"stream: rate and counter, no track", 80, 0, false, "LAN",
			barLayout{rate: true, counter: true, route: true}},
	}
	for _, c := range cases {
		if got := layout(c.width, c.total, c.showNames, c.route); got != c.want {
			t.Errorf("%s: layout(%d, %d, %v, %q) = %+v, want %+v",
				c.name, c.width, c.total, c.showNames, c.route, got, c.want)
		}
	}
}

// Whatever the width, the booked elements fit the row: mpb never has to
// truncate a decorator (and with it a number).
func TestLayoutNeverOverflows(t *testing.T) {
	for _, total := range []int64{0, 900_000, 100_000_000, 23_456_789_000} {
		for w := 20; w <= 220; w++ {
			l := layout(w, total, true, "local network")
			used := pctFurniture
			if l.eta {
				used += etaFurniture
			}
			if l.rate {
				used += rateFurniture
			}
			if l.counter {
				used += counterFurniture(total)
			}
			if l.nameCap > 0 {
				used += 5 + l.nameCap
			}
			if l.bar > 0 {
				used += barFurniture + l.bar
			}
			if l.route {
				used += len("local network") + 5
			}
			if used > max(w, pctFurniture) {
				t.Fatalf("width %d total %d: layout %+v uses %d columns", w, total, l, used)
			}
		}
	}
}

// A failing width query keeps the fallback, never collapses to zero.
func TestTerminalWidthFallback(t *testing.T) {
	silenceStderr(t)
	if got := terminalWidth(42); got != 42 {
		t.Errorf("terminalWidth(42) = %d on a non-terminal, want fallback 42", got)
	}
}

// The counter must be the same width on every frame: mpb sizes the bar off
// what the decorators leave, so a counter that gained a column ("44 MB" vs
// "43.8 MB") would visibly resize the bar mid-transfer.
func TestCounterLineWidthIsStable(t *testing.T) {
	for _, total := range []int64{999, 100_000_000, 1_000_000_000, 23_456_789_000} {
		want := len(counterLine(0, total))
		for cur := int64(0); cur <= total; cur += total/997 + 1 {
			if got := len(counterLine(cur, total)); got != want {
				t.Fatalf("counterLine(%d, %d) = %q (%d wide), want %d",
					cur, total, counterLine(cur, total), got, want)
			}
		}
	}
	// Unknown total (streaming): no bar track, so only the figure renders.
	if got := counterLine(1234, 0); got != "1.2 KB" {
		t.Errorf("counterLine with no total = %q", got)
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
	if !strings.HasSuffix(got, "/file.tar.gz") || !strings.HasPrefix(got, "a-very") || !strings.Contains(got, "…") {
		t.Errorf("truncation must keep the head and the whole basename: %q", got)
	}
	// A basename too long for the cap is cut in its middle, keeping its
	// start and extension.
	if got := truncateName("assets/video-final-cut.bin", 12); got != "vide…cut.bin" {
		t.Errorf("long basename = %q, want a middle cut", got)
	}
	// Degenerate caps must not panic on the r[:max-tail-1] slice.
	for _, max := range []int{-1, 0, 1, 2, 3} {
		got := truncateName("abcdef", max) // len 6 > every max here, so it truncates
		if r := []rune(got); len(r) > max && max > 0 {
			t.Errorf("truncateName(_, %d) = %q (%d runes), exceeds cap", max, got, len(r))
		}
	}
}
