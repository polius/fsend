package uxlog

import (
	"bytes"
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

func TestColorFor(t *testing.T) {
	var buf bytes.Buffer

	t.Setenv("NO_COLOR", "")
	t.Setenv("FORCE_COLOR", "")
	if ColorFor(&buf) {
		t.Error("ColorFor(non-TTY) = true with no env override")
	}

	t.Setenv("FORCE_COLOR", "1")
	if !ColorFor(&buf) {
		t.Error("ColorFor(non-TTY) = false despite FORCE_COLOR=1")
	}

	t.Setenv("NO_COLOR", "1")
	if ColorFor(&buf) {
		t.Error("NO_COLOR should win over FORCE_COLOR in ColorFor")
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

func TestNearest256(t *testing.T) {
	// The fallback must be the closest cell. The lime green in particular
	// used to point at 114 (a mint cell); nearest is 149.
	cases := map[string]string{
		"#9ece6a": "149", // success lime
		"#e0af68": "179", // warning
		"#7dcfff": "117", // info
		"#000000": "0",
		"#ffffff": "15",
	}
	for hex, want := range cases {
		if got := nearest256(hex); got != want {
			t.Errorf("nearest256(%s) = %s, want %s", hex, got, want)
		}
	}
}

func TestDarkBackground(t *testing.T) {
	cases := []struct {
		theme, colorfgbg string
		wantDark         bool
	}{
		{"", "", true},           // unset → designed-for default
		{"light", "", false},     // explicit override
		{"dark", "", true},       // explicit override
		{"light", "15;0", false}, // explicit beats COLORFGBG
		{"", "15;0", true},       // fg 15, bg 0 → dark
		{"", "0;15", false},      // bg 15 → light
		{"", "0;7", false},       // bg 7 (white) → light
		{"", "0;8", true},        // bg 8 (bright black) → dark
		{"", "garbage", true},    // unparseable → dark
	}
	for _, c := range cases {
		t.Setenv("FSEND_THEME", c.theme)
		t.Setenv("COLORFGBG", c.colorfgbg)
		resetThemeForTesting()
		if got := darkBackground(); got != c.wantDark {
			t.Errorf("FSEND_THEME=%q COLORFGBG=%q: darkBackground()=%v, want %v",
				c.theme, c.colorfgbg, got, c.wantDark)
		}
	}
}

func TestTokenPicksBackgroundSibling(t *testing.T) {
	t.Setenv("COLORFGBG", "")
	t.Setenv("FSEND_THEME", "dark")
	resetThemeForTesting()
	if got := tokenAccent.hex(); got != "#ff9e64" {
		t.Errorf("dark accent = %s, want #ff9e64", got)
	}
	t.Setenv("FSEND_THEME", "light")
	resetThemeForTesting()
	if got := tokenAccent.hex(); got != "#b15c00" {
		t.Errorf("light accent = %s, want #b15c00", got)
	}
	if tokenAccent.dark == tokenAccent.light {
		t.Error("dark and light siblings must differ")
	}
}
