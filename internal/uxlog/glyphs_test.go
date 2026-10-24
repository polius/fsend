package uxlog

import (
	"bytes"
	"math"
	"strconv"
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

func TestCurrentTheme(t *testing.T) {
	cases := []struct {
		theme, colorfgbg string
		want             theme
	}{
		{"", "", themeUnknown},        // nothing known → the "both" palette
		{"light", "", themeLight},     // explicit override
		{"dark", "", themeDark},       // explicit override
		{"light", "15;0", themeLight}, // explicit beats COLORFGBG
		{"", "15;0", themeDark},       // fg 15, bg 0 → dark
		{"", "0;15", themeLight},      // bg 15 → light
		{"", "0;7", themeLight},       // bg 7 (white) → light
		{"", "0;8", themeDark},        // bg 8 (bright black) → dark
		{"", "garbage", themeUnknown}, // unparseable → unknown
	}
	for _, c := range cases {
		t.Setenv("FSEND_THEME", c.theme)
		t.Setenv("COLORFGBG", c.colorfgbg)
		resetThemeForTesting()
		if got := currentTheme(); got != c.want {
			t.Errorf("FSEND_THEME=%q COLORFGBG=%q: currentTheme()=%v, want %v",
				c.theme, c.colorfgbg, got, c.want)
		}
	}
}

func TestTokenPicksBackgroundSibling(t *testing.T) {
	t.Setenv("COLORFGBG", "")
	for _, c := range []struct{ theme, want string }{
		{"dark", "#ff9e64"}, {"light", "#b15c00"}, {"", "#c85000"},
	} {
		t.Setenv("FSEND_THEME", c.theme)
		resetThemeForTesting()
		if got := tokenAccent.hex(); got != c.want {
			t.Errorf("FSEND_THEME=%q accent = %s, want %s", c.theme, got, c.want)
		}
	}
}

// TestPaletteContrast pins WCAG AA (4.5:1) for every role: dark against
// black, light against white, and the unknown-background palette against
// both — in truecolour and in the 256-colour cells actually emitted.
func TestPaletteContrast(t *testing.T) {
	const aa = 4.5
	white, black := relLum(255, 255, 255), relLum(0, 0, 0)
	tokens := map[string]colourToken{
		"accent": tokenAccent, "primary": tokenPrimary, "success": tokenSuccess,
		"warning": tokenWarning, "error": tokenError, "info": tokenInfo,
	}
	check := func(name, variant string, r, g, b int, bgs ...float64) {
		l := relLum(r, g, b)
		for _, bg := range bgs {
			if c := contrast(l, bg); c < aa {
				t.Errorf("%s %s: contrast %.2f < %.1f", name, variant, c, aa)
			}
		}
	}
	for name, tok := range tokens {
		r, g, b := parseHexInts(tok.dark)
		check(name, "dark", r, g, b, black)
		r, g, b = cube256(atoiOrDie(t, nearest256(tok.dark)))
		check(name, "dark/256", r, g, b, black)
		r, g, b = parseHexInts(tok.light)
		check(name, "light", r, g, b, white)
		r, g, b = cube256(atoiOrDie(t, nearest256(tok.light)))
		check(name, "light/256", r, g, b, white)
		r, g, b = parseHexInts(tok.both)
		check(name, "both", r, g, b, white, black)
		if tok.both256 != "" {
			r, g, b = cube256(atoiOrDie(t, tok.both256))
			check(name, "both/256", r, g, b, white, black)
		}
	}
}

func TestUnknownTheme256FallsBackToDefaultForeground(t *testing.T) {
	t.Setenv("FSEND_THEME", "")
	t.Setenv("COLORFGBG", "")
	t.Setenv("COLORTERM", "")
	resetThemeForTesting()
	if got := fg(tokenAccent); got != "" {
		t.Errorf("accent under 256/unknown = %q, want default fg", got)
	}
	if got := fgBold(tokenAccent); got != colorBold {
		t.Errorf("bold accent under 256/unknown = %q, want plain bold", got)
	}
	if got := fg(tokenInfo); got != "\x1b[38;5;63m" {
		t.Errorf("info under 256/unknown = %q", got)
	}
}

func TestPlain(t *testing.T) {
	for _, c := range []struct {
		plain, lcAll, lang string
		want               bool
	}{
		{"", "", "", false},
		{"1", "", "", true},
		{"0", "", "", false},
		{"", "C", "en_US.UTF-8", true}, // LC_ALL wins
		{"", "", "en_US.ISO8859-1", true},
		{"", "", "en_US.utf8", false},
		{"", "", "C.UTF-8", false},
	} {
		t.Setenv("FSEND_PLAIN", c.plain)
		t.Setenv("LC_ALL", c.lcAll)
		t.Setenv("LC_CTYPE", "")
		t.Setenv("LANG", c.lang)
		if got := Plain(); got != c.want {
			t.Errorf("FSEND_PLAIN=%q LC_ALL=%q LANG=%q: Plain()=%v, want %v",
				c.plain, c.lcAll, c.lang, got, c.want)
		}
	}
}

func relLum(r, g, b int) float64 {
	lin := func(c int) float64 {
		v := float64(c) / 255
		if v <= 0.04045 {
			return v / 12.92
		}
		return math.Pow((v+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

func contrast(a, b float64) float64 {
	return (math.Max(a, b) + 0.05) / (math.Min(a, b) + 0.05)
}

func atoiOrDie(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}
