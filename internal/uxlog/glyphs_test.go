package uxlog

import (
	"strings"
	"testing"
)

func TestMarker_NonTTYFallback(t *testing.T) {
	// In `go test` stderr isn't a real TTY, so we should get the ASCII
	// fallback labels every time.
	cases := []struct {
		fn   func() string
		name string
		want string
	}{
		{Check, "Check", "[OK]"},
		{Cross, "Cross", "FAIL"},
		{Warn, "Warn", "[!]"},
		{Info, "Info", "[i]"},
		{Retry, "Retry", "[~]"},
		{Spin, "Spin", "[*]"},
	}
	for _, c := range cases {
		if got := c.fn(); got != c.want {
			t.Errorf("%s fallback = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSeparator_FitsBudget(t *testing.T) {
	got := Separator()
	if l := visualLen(got); l < 20 || l > 60 {
		t.Errorf("separator length = %d, want between 20 and 60", l)
	}
	// Non-TTY (test env) → ASCII rule.
	if strings.ContainsRune(got, '─') {
		t.Errorf("separator should be ASCII under non-TTY, got %q", got)
	}
}

// visualLen counts runes — strings.Repeat builds rune-count == byte-count
// for both '-' and '─' (the latter is 3 bytes).
func visualLen(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}

func TestColorEnabled_RespectsNoColor(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	resetColorForTesting()
	if colorEnabled() {
		t.Error("colorEnabled() = true when NO_COLOR is set")
	}
}

func TestColorEnabled_ForceColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "1")
	resetColorForTesting()
	if !colorEnabled() {
		t.Error("colorEnabled() = false despite FORCE_COLOR=1")
	}
}

func TestColorEnabled_NoColorBeatsForceColor(t *testing.T) {
	// NO_COLOR is the de-facto standard; if it's set (non-empty), it
	// must win over FORCE_COLOR. The colour table at no-color.org is
	// explicit about this precedence.
	t.Setenv("NO_COLOR", "1")
	t.Setenv("FORCE_COLOR", "1")
	resetColorForTesting()
	if colorEnabled() {
		t.Error("NO_COLOR should win over FORCE_COLOR")
	}
}
