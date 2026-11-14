package uxlog

import (
	"bytes"
	"math"
	"runtime"
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
		{Ask, "Ask", "[?]"},
		{SendArrow, "SendArrow", " "},
		{RecvArrow, "RecvArrow", " "},
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
		"#e5c07b": "180", // warning
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
		{"dark", "#e0805a"}, {"light", "#b54c22"}, {"", ""},
	} {
		t.Setenv("FSEND_THEME", c.theme)
		resetThemeForTesting()
		if got := tokenAccent.hex(); got != c.want {
			t.Errorf("FSEND_THEME=%q accent = %q, want %q", c.theme, got, c.want)
		}
	}
}

// The dark accent is the logo orange: one brand colour.
func TestDarkAccentIsTheLogo(t *testing.T) {
	if tokenAccent.dark != "#e0805a" {
		t.Errorf("dark accent = %s, want the logo's #e0805a", tokenAccent.dark)
	}
}

// TestPaletteContrast pins WCAG AA (4.5:1) for every role on each theme's
// usual backgrounds — dark against black, VS Code, One Dark, Solarized and
// Tokyo Night; light against white, Solarized, GitHub-ish off-white,
// Catppuccin Latte and Gruvbox — in truecolour and in the 256-colour cells
// actually emitted (black / white only for those: the cube is coarse).
func TestPaletteContrast(t *testing.T) {
	const aa = 4.5
	lum := func(hex string) float64 { r, g, b := parseHexInts(hex); return relLum(r, g, b) }
	darkBGs := []string{"#000000", "#1e1e1e", "#282c34", "#002b36", "#1a1b26"}
	lightBGs := []string{"#ffffff", "#fdf6e3", "#fafafa", "#eff1f5", "#fbf1c7"}
	tokens := map[string]colourToken{
		"accent": tokenAccent, "primary": tokenPrimary, "success": tokenSuccess,
		"warning": tokenWarning, "error": tokenError, "info": tokenInfo,
	}
	check := func(name, variant string, r, g, b int, bgs ...string) {
		l := relLum(r, g, b)
		for _, bg := range bgs {
			if c := contrast(l, lum(bg)); c < aa {
				t.Errorf("%s %s on %s: contrast %.2f < %.1f", name, variant, bg, c, aa)
			}
		}
	}
	for name, tok := range tokens {
		r, g, b := parseHexInts(tok.dark)
		check(name, "dark", r, g, b, darkBGs...)
		r, g, b = cube256(atoiOrDie(t, nearest256(tok.dark)))
		check(name, "dark/256", r, g, b, "#000000")
		r, g, b = parseHexInts(tok.light)
		check(name, "light", r, g, b, lightBGs...)
		r, g, b = cube256(atoiOrDie(t, nearest256(tok.light)))
		check(name, "light/256", r, g, b, "#ffffff")
	}
}

// With the background unknown, every role uses the terminal's own ANSI
// colours (SGR 31 to 36), whatever COLORTERM says: the user's theme has
// tuned those for its background.
func TestUnknownThemeUsesANSI(t *testing.T) {
	t.Setenv("FSEND_THEME", "")
	t.Setenv("COLORFGBG", "")
	for _, ct := range []string{"", "truecolor"} {
		t.Setenv("COLORTERM", ct)
		resetThemeForTesting()
		for tok, want := range map[*colourToken]string{
			&tokenAccent: "\x1b[36m", &tokenPrimary: "\x1b[35m", &tokenSuccess: "\x1b[32m",
			&tokenWarning: "\x1b[33m", &tokenError: "\x1b[31m", &tokenInfo: "\x1b[36m",
		} {
			if got := fg(*tok); got != want {
				t.Errorf("COLORTERM=%q: fg = %q, want %q", ct, got, want)
			}
		}
		if got := fgBold(tokenAccent); got != "\x1b[1;36m" {
			t.Errorf("COLORTERM=%q: bold accent = %q", ct, got)
		}
	}
}

// A declared background without truecolour maps to the nearest 256 cell.
func TestKnownTheme256(t *testing.T) {
	t.Setenv("FSEND_THEME", "dark")
	t.Setenv("COLORTERM", "")
	resetThemeForTesting()
	if got := fg(tokenAccent); got != "\x1b[38;5;"+nearest256("#e0805a")+"m" {
		t.Errorf("accent under 256/dark = %q", got)
	}
}

// The OSC 11 reply is parsed in every form terminals send it: 1 to 4 hex
// digits a channel, ended by BEL or ST, with the DA1 reply after it.
func TestParseOSC11(t *testing.T) {
	for _, c := range []struct {
		reply string
		want  theme
		ok    bool
	}{
		{"\x1b]11;rgb:0000/0000/0000\x1b\\\x1b[?62;22c", themeDark, true},
		{"\x1b]11;rgb:ffff/ffff/ffff\x07", themeLight, true},
		{"\x1b]11;rgb:28/2c/34\x1b\\", themeDark, true},        // One Dark
		{"\x1b]11;rgb:fdfd/f6f6/e3e3\x1b\\", themeLight, true}, // Solarized light
		{"\x1b]11;rgb:f/f/f\x07", themeLight, true},
		{"\x1b[?62;22c", themeUnknown, false}, // no OSC 11 support: DA1 only
		{"", themeUnknown, false},
		{"\x1b]11;rgb:zz/00/00\x07", themeUnknown, false},
		{"\x1b]11;rgb:00/00\x07", themeUnknown, false},
	} {
		th, ok := parseOSC11([]byte(c.reply))
		if th != c.want || ok != c.ok {
			t.Errorf("parseOSC11(%q) = %v, %v; want %v, %v", c.reply, th, ok, c.want, c.ok)
		}
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
		// Windows consoles ignore the POSIX locale variables.
		if runtime.GOOS == "windows" && c.plain == "" {
			c.want = false
		}
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
