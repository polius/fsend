package uxlog

import (
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
		{Cross, "Cross", "[FAIL]"},
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
