package uxlog

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// We can't easily inject stderr or fake a TTY in unit tests, so the
// spinner tests below exercise the non-TTY path (which is what the
// `go test` harness gives us anyway) plus the concurrency-safety
// properties of Stop.

func TestStartSpinner_NonTTY_PrintsStaticLineAndStopIsNilSafe(t *testing.T) {
	// In test runs stderr is not a TTY, so StartSpinner takes the
	// non-TTY branch: prints one static line and Stop is a visual no-op
	// but must still be safe.
	s := StartSpinner("hello")
	if s == nil {
		t.Fatal("StartSpinner returned nil")
	}
	s.Stop() // first stop
	s.Stop() // double-stop must not panic or block
}

// The optional hint rides after the message (and, on a TTY, after the
// elapsed chip) so a long wait advertises its escape hatch; the non-TTY
// static line records it too.
func TestSpinner_HintFollowsMessage(t *testing.T) {
	var buf bytes.Buffer
	s := &Spinner{msg: "Waiting for receiver", hint: HintCancel, w: &buf}
	s.draw("⠋")
	out := buf.String()
	if !strings.Contains(out, "Waiting for receiver") || !strings.Contains(out, HintCancel) {
		t.Fatalf("draw missing message or hint: %q", out)
	}
	if strings.Index(out, "Waiting for receiver") > strings.Index(out, HintCancel) {
		t.Errorf("hint must follow the message: %q", out)
	}

	// Non-TTY logs drop the hint: nobody reading a log can press Ctrl-C.
	buf.Reset()
	nonTTY := &Spinner{msg: "first", hint: HintCancel, w: &buf}
	nonTTY.SetMessage("second")
	if got := buf.String(); !strings.Contains(got, "second") || strings.Contains(got, HintCancel) {
		t.Errorf("non-TTY line = %q, want the message without the hint", got)
	}
}

// A notice printed while a spinner animates must start on a cleared line,
// not glued to the end of the current frame.
func TestPrintln_ClearsLiveSpinnerLine(t *testing.T) {
	var buf bytes.Buffer
	s := &Spinner{msg: "Waiting", w: &buf, tty: true}
	setActiveSpinner(s)
	defer clearActiveSpinner(s)
	s.draw("⠋")
	printlnTo(&buf, "Cancelling…")
	out := buf.String()
	i := strings.Index(out, "Cancelling…")
	if i < 0 || !strings.HasSuffix(out[:i], "\r\x1b[2K") {
		t.Errorf("notice not preceded by a line clear: %q", out)
	}
}

func TestSpinner_NilReceiverStopIsNoOp(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil Stop panicked: %v", r)
		}
	}()
	var s *Spinner
	s.Stop()
}

func TestSpinner_ConcurrentStop(t *testing.T) {
	s := StartSpinner("x")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.Stop()
		}()
	}
	wg.Wait()
}

// frameDrawWritesCRandClear is a white-box check that draw writes both
// the carriage return and the erase-line escape so the spinner can never
// leave fragments of a previous longer line on screen.
func TestSpinner_DrawFormatIncludesEraseLine(t *testing.T) {
	// We replicate the draw path with a captured writer so we can
	// inspect the byte sequence without owning a TTY.
	var buf bytes.Buffer
	s := &Spinner{msg: "msg", w: &buf}
	s.draw("⠋")
	out := buf.String()
	if !strings.HasPrefix(out, "\r\x1b[2K") {
		t.Fatalf("draw output missing \\r + erase-line prefix: %q", out)
	}
	if !strings.Contains(out, "msg") {
		t.Fatalf("draw output missing message: %q", out)
	}
}

func TestSpinner_SetMessage(t *testing.T) {
	// Nil-safe, like Stop.
	var nilSpin *Spinner
	nilSpin.SetMessage("x")

	// Non-TTY: the new message is printed as a fresh static line so log
	// readers see the state change; a same-message call prints nothing.
	var buf bytes.Buffer
	s := &Spinner{msg: "first", w: &buf}
	s.SetMessage("second")
	if got := buf.String(); !strings.Contains(got, "second") {
		t.Errorf("non-TTY SetMessage should print the new line, got %q", got)
	}
	before := buf.Len()
	s.SetMessage("second")
	if buf.Len() != before {
		t.Errorf("unchanged message must not reprint: %q", buf.String())
	}

	// The next draw uses the new message.
	buf.Reset()
	s.draw("⠋")
	if got := buf.String(); !strings.Contains(got, "second") || strings.Contains(got, "first") {
		t.Errorf("draw after SetMessage = %q", got)
	}
}

// A spinner line wider than the terminal wraps, and every \r redraw then
// leaves a row behind. fitSpinner keeps it inside width-1 columns: the
// hint goes first, then the message's tail; the elapsed time stays.
func TestFitSpinner(t *testing.T) {
	const pw = "Waiting for Pols-MacBook-Pro to enter the password"
	cases := []struct {
		msg, elapsed, hint string
		width              int
		want               string
	}{
		{"Waiting for receiver", "4 s", HintCancel, 100, "Waiting for receiver  ·  4 s  ·  Ctrl-C to cancel"},
		{"Waiting for receiver", "4 s", HintCancel, 40, "Waiting for receiver  ·  4 s"},
		{"Waiting for receiver", "", HintCancel, 40, "Waiting for receiver"},
		{pw, "11 s", HintCancel, 80, pw + "  ·  11 s"},
		{pw, "11 s", HintCancel, 44, "Waiting for Pols-MacBook-Pro to…  ·  11 s"},
		{"Waiting for receiver", "4 s", HintCancel, 12, "Waiting…"},
	}
	for _, c := range cases {
		got := fitSpinner(c.msg, c.elapsed, c.hint, c.width)
		if got != c.want {
			t.Errorf("fitSpinner(%q, %d) = %q, want %q", c.msg, c.width, got, c.want)
		}
		if n := utf8.RuneCountInString(got) + 2; n > c.width-1 {
			t.Errorf("fitSpinner(%q, %d): line is %d columns", c.msg, c.width, n)
		}
	}
}

// Static lines wrap at word boundaries under the indent; off a terminal
// they stay on one line.
func TestWrapOffTerminal(t *testing.T) {
	const s = "The code works for one receiver, while this stays open."
	if got := Wrap(s, 2); got != "  "+s {
		t.Errorf("Wrap off a terminal = %q", got)
	}
}
