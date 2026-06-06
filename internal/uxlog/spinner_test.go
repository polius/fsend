package uxlog

import (
	"bytes"
	"strings"
	"sync"
	"testing"
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
	s.Stop()  // first stop
	s.Stop()  // double-stop must not panic or block
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
