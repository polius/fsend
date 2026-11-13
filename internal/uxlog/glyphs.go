package uxlog

import (
	"io"
	"math"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// Glyph kinds for status lines. Each maps to a unicode + ASCII fallback
// pair and a colour. Centralising them here keeps every CLI surface
// (send, receive, connect, uninstall, errors) consistent.
//
// The set is deliberately small and limited to symbols every monospace
// font carries: ✓ ✗ ! ? → ← and the spinner. A glyph missing from the
// user's font is drawn from a fallback (often the colour-emoji font) with
// a different width and baseline, so ⚠ ℹ ⟳ ⇢ ⇣ are not used.
type glyphKind int

const (
	gCheck glyphKind = iota // ✓ / [OK]
	gCross                  // ✗ / [FAIL]
	gWarn                   // ! (bold) / [!]
	gInfo                   // no glyph — a blank gutter / [i]
	gStep                   // › / >  (narration marker, as the install scripts print)
	gRetry                  // ! (bold) / [~]
	gAsk                    // ? (bold) / [?]
	gSpin                   // … / [*]  ("waiting" — non-animated; deliberately not a
	//                                   single-frame braille spinner)
)

// Marker returns the leading status glyph appropriate for stderr's TTY
// state. Caller appends the rest of the line.
//
// Every glyph is one column wide and sits in column 0, so the text after
// "<glyph> " starts in column 2 — the grid every line follows (see Line).
// When stderr is a real terminal we render the unicode glyph and (when
// allowed) apply colour. When stderr is a pipe/file we render the ASCII
// fallback with no colour so log files stay readable.
func Marker(kind glyphKind) string {
	utf8, ascii, token, bold := glyphForKind(kind)
	if !renderTTY(os.Stderr) {
		return ascii
	}
	if !colorEnabled() {
		if bold && utf8 != " " {
			return colorBold + utf8 + colorReset
		}
		return utf8
	}
	if bold {
		return fgBold(token) + utf8 + colorReset
	}
	if c := fg(token); c != "" {
		return c + utf8 + colorReset
	}
	return utf8
}

// Check returns the success glyph (✓ or [OK]).
func Check() string { return Marker(gCheck) }

// Cross returns the failure glyph (✗ or [FAIL]).
func Cross() string { return Marker(gCross) }

// Warn returns the warning glyph (a bold ! or [!]).
func Warn() string { return Marker(gWarn) }

// Info returns the gutter for a neutral notice: blank on a terminal, so the
// notice's text lines up in column 2 with no symbol of its own; "[i]" in
// logs, where a marker keeps lines greppable.
func Info() string { return Marker(gInfo) }

// Step returns the step glyph (› or >) — the marker the install scripts
// (scripts/install.sh, install.ps1) print before each action they take.
// fsend uses it for its own narration ahead of a re-run installer
// (--update) so the two outputs read as one continuous block.
func Step() string { return Marker(gStep) }

// Retry returns the retry glyph: the warning ! on a terminal (a dropped
// connection is a soft warning; the spinner that follows shows the wait),
// "[~]" in logs.
func Retry() string { return Marker(gRetry) }

// Ask returns the prompt glyph (a bold ? or [?]) for lines that want an
// answer, so a scan down the gutter finds every question.
func Ask() string { return Marker(gAsk) }

// SendArrow and RecvArrow mark the direction of a transfer in the gutter of
// its opening line (→ sending, ← receiving). Pipes get a blank gutter: the
// words say it, and scraped output stays ASCII.
func SendArrow() string { return arrow("→") }

// RecvArrow is SendArrow's receiving counterpart.
func RecvArrow() string { return arrow("←") }

func arrow(a string) string {
	if !renderTTY(os.Stderr) {
		return " " // a blank gutter keeps the text in column 2
	}
	if colorEnabled() {
		if c := fg(tokenAccent); c != "" {
			return c + a + colorReset
		}
	}
	return a
}

// PasswordChip renders the "password required" chip on an artifact line:
// text in the warning colour, no glyph (glyphs live only in the gutter).
// Plain text on pipes/files like every other marker here.
func PasswordChip() string {
	if !renderTTY(os.Stderr) {
		return "[password required]"
	}
	if colorEnabled() {
		if c := fg(tokenWarning); c != "" {
			return c + "password required" + colorReset
		}
	}
	return "password required"
}

func glyphForKind(k glyphKind) (utf8, ascii string, token colourToken, bold bool) {
	switch k {
	case gCheck:
		// Green means "it worked" and nothing else: the tick is the only
		// element that wears it.
		return "✓", "[OK]", tokenSuccess, false
	case gCross:
		return "✗", "[FAIL]", tokenError, false
	case gWarn:
		return "!", "[!]", tokenWarning, true
	case gInfo:
		// Neutral notices carry no symbol: an empty gutter, text in
		// column 2 like every other line.
		return " ", "[i]", colourToken{}, false
	case gStep:
		// Default foreground: narration, not information. The › is the
		// same glyph the install scripts print for every step, so
		// --update's lines and the installer output that follows them
		// match.
		return "›", ">", colourToken{}, false
	case gRetry:
		return "!", "[~]", tokenWarning, true
	case gAsk:
		return "?", "[?]", tokenPrimary, true
	case gSpin:
		// The animated spinner glyph (Spinner type) carries its own
		// rendering; this static fallback is only used in the rare
		// non-animated paths (e.g. tests).
		return "…", "[*]", tokenAccent, false
	}
	return "", "", colourToken{}, false
}

// ---------------------------------------------------------------------
// Colour handling
// ---------------------------------------------------------------------

// colourToken is one semantic colour role. The roles mirror the theme keys
// opencode's theme system defines (primary / accent / success / warning /
// error / info), so the CLI vocabulary and the TUI vocabulary stay
// parallel:
//
//	accent   — act: the share code, the spinner, the filled bar, arrows
//	primary  — decide: the ? gutter and the questions it leads
//	success  — the ✓ tick, and nothing else
//	warning  — caution (!, "differs", "deletes yours", a stall)
//	error    — failure (✗)
//	info     — --help flags and links
//
// Which colour a role gets depends on what fsend knows about the
// background:
//
//   - Unknown (the usual case): the terminal's own ANSI colours, SGR 31
//     to 36. The user's theme has already tuned those for its background
//     and for their eyes — the reason gh, cargo, git and docker use them.
//   - Dark or light, declared (FSEND_THEME, COLORFGBG) or detected (an
//     OSC 11 query, see bgquery_*.go): a truecolour palette that clears
//     WCAG AA (4.5:1) on that theme's usual backgrounds, or the nearest
//     xterm-256 cell when the terminal lacks truecolour.
//
// The dark accent is the logo orange (#E0805A); the light one is the same
// hue darkened to clear 4.5:1 on common light backgrounds. Warning sits
// towards yellow so it never reads as a second accent. The zero token
// means "no colour".
type colourToken struct {
	dark, light string // "#rrggbb"
	ansi        string // SGR foreground parameter when the background is unknown
}

var (
	tokenAccent  = colourToken{"#e0805a", "#b54c22", "36"}
	tokenPrimary = colourToken{"#bb9af7", "#7847bd", "35"}
	tokenSuccess = colourToken{"#9ece6a", "#587539", "32"}
	tokenWarning = colourToken{"#e5c07b", "#7d6500", "33"}
	tokenError   = colourToken{"#f7768e", "#b3261e", "31"}
	tokenInfo    = colourToken{"#7dcfff", "#007197", "36"}
)

const (
	colorReset = "\x1b[0m"
	colorBold  = "\x1b[1m"
	// colorFaint is SGR 2. The terminal derives faint from the user's own
	// foreground, so it stays readable wherever that is — unlike a fixed
	// grey, or bright-black, which some themes map close to the
	// background. Terminals that ignore it show default text.
	colorFaint = "\x1b[2m"
)

// truecolorEnabled reports whether the terminal advertised 24-bit colour
// support via COLORTERM (truecolor | 24bit) — the check opencode's docs
// prescribe before using full-palette themes. Cached: env is fixed for
// the process lifetime.
var (
	tcOnce sync.Once
	tcOK   bool
)

func truecolorEnabled() bool {
	tcOnce.Do(func() {
		switch os.Getenv("COLORTERM") {
		case "truecolor", "24bit", "TrueColor", "24BIT":
			tcOK = true
		}
	})
	return tcOK
}

// sgr returns the SGR foreground parameters for the token: the ANSI
// colour when the background is unknown, else "38;2;r;g;b" under
// truecolour or "38;5;<idx>". "" means the default foreground.
func sgr(t colourToken) string {
	th := currentTheme()
	if th == themeUnknown {
		return t.ansi
	}
	hex := t.hexFor(th)
	if hex == "" {
		return ""
	}
	if truecolorEnabled() {
		r, g, b := parseHex(hex)
		return "38;2;" + r + ";" + g + ";" + b
	}
	return "38;5;" + nearest256(hex)
}

// hex is the token's truecolour for the terminal's background ("" when
// the background is unknown).
func (t colourToken) hex() string { return t.hexFor(currentTheme()) }

func (t colourToken) hexFor(th theme) string {
	switch th {
	case themeDark:
		return t.dark
	case themeLight:
		return t.light
	}
	return ""
}

type theme int

const (
	themeUnknown theme = iota
	themeDark
	themeLight
)

// currentTheme reports which background the palette targets:
//
//  1. FSEND_THEME=light|dark — explicit, documented override.
//  2. COLORFGBG — the de-facto var iTerm2/Konsole/rxvt export as
//     "<fg>;<bg>"; the last field is the background's ANSI colour index.
//     Index 7 (white) and 9..15 (bright) are the light end.
//  3. An OSC 11 query to the terminal, when stderr is a colour terminal
//     (queryBackground; bounded, and answered or abandoned in a blink).
//  4. unknown → the terminal's own ANSI colours.
var (
	bgOnce  sync.Once
	bgTheme theme
)

func currentTheme() theme {
	bgOnce.Do(func() {
		switch {
		case strings.EqualFold(os.Getenv("FSEND_THEME"), "light"):
			bgTheme = themeLight
		case strings.EqualFold(os.Getenv("FSEND_THEME"), "dark"):
			bgTheme = themeDark
		default:
			bgTheme = themeUnknown
			if c := os.Getenv("COLORFGBG"); c != "" {
				if i := strings.LastIndexByte(c, ';'); i >= 0 {
					if bg, err := strconv.Atoi(c[i+1:]); err == nil {
						bgTheme = themeDark
						if bg == 7 || bg >= 9 {
							bgTheme = themeLight
						}
					}
					return
				}
			}
			if queryAllowed() {
				bgTheme = queryBackground()
			}
		}
	})
	return bgTheme
}

// DetectTheme settles the background question up front. Callers run it
// before starting goroutines that draw (a spinner), so the one-off
// terminal query never races a redraw or a prompt's read. Idempotent.
func DetectTheme() { currentTheme() }

// queryAllowed gates the OSC 11 query: only for a colour-capable stderr
// terminal — the answer only picks colours — and never in plain mode
// (renderTTY covers it, and TERM=dumb).
func queryAllowed() bool {
	return colorEnabled() && renderTTY(os.Stderr)
}

// themeFromRGB classifies a background by its relative luminance: above
// the midpoint is a light theme. Channels are 0..1.
func themeFromRGB(r, g, b float64) theme {
	lin := func(c float64) float64 {
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	if 0.2126*lin(r)+0.7152*lin(g)+0.0722*lin(b) > 0.18 {
		return themeLight
	}
	return themeDark
}

// parseOSC11 extracts the background colour from a terminal's reply to
// "OSC 11 ; ?" — "\x1b]11;rgb:RRRR/GGGG/BBBB" ended by BEL or ST, with 1 to
// 4 hex digits per channel. ok=false when the reply holds no such colour.
func parseOSC11(reply []byte) (th theme, ok bool) {
	s := string(reply)
	i := strings.Index(s, "]11;rgb:")
	if i < 0 {
		return themeUnknown, false
	}
	s = s[i+len("]11;rgb:"):]
	if j := strings.IndexAny(s, "\x07\x1b"); j >= 0 {
		s = s[:j]
	}
	parts := strings.Split(s, "/")
	if len(parts) != 3 {
		return themeUnknown, false
	}
	var ch [3]float64
	for k, p := range parts {
		if len(p) < 1 || len(p) > 4 {
			return themeUnknown, false
		}
		v, err := strconv.ParseUint(p, 16, 16)
		if err != nil {
			return themeUnknown, false
		}
		ch[k] = float64(v) / float64(uint64(1)<<(4*len(p))-1)
	}
	return themeFromRGB(ch[0], ch[1], ch[2]), true
}

// nearest256 returns the xterm-256 palette index closest to hex (plain
// RGB distance — the tokens sit far apart, so a perceptual metric agrees).
// Deriving it beats hand-picking: a preset index silently drifts from the
// palette it approximates (the old lime green pointed at a mint cell).
func nearest256(hex string) string {
	hr, hg, hb := parseHexInts(hex)
	best, bestD := 0, 1<<30
	for i := 0; i < 256; i++ {
		r, g, b := cube256(i)
		d := (r-hr)*(r-hr) + (g-hg)*(g-hg) + (b-hb)*(b-hb)
		if d < bestD {
			best, bestD = i, d
		}
	}
	return itoa(best)
}

// cube256 maps an xterm-256 index to its RGB: the 16 system colours, the
// 6×6×6 colour cube, then the 24-step greyscale ramp.
func cube256(i int) (r, g, b int) {
	if i < 16 {
		sys := [16][3]int{
			{0, 0, 0}, {128, 0, 0}, {0, 128, 0}, {128, 128, 0},
			{0, 0, 128}, {128, 0, 128}, {0, 128, 128}, {192, 192, 192},
			{128, 128, 128}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0},
			{0, 0, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255},
		}
		return sys[i][0], sys[i][1], sys[i][2]
	}
	if i < 232 {
		i -= 16
		lv := [6]int{0, 95, 135, 175, 215, 255}
		return lv[i/36], lv[(i/6)%6], lv[i%6]
	}
	v := 8 + (i-232)*10
	return v, v, v
}

// fg renders s in the token's colour; caller wraps with colorReset (or
// uses the gated colour helpers in format.go). "" for the default fg.
func fg(t colourToken) string {
	if p := sgr(t); p != "" {
		return "\x1b[" + p + "m"
	}
	return ""
}

// fgBold renders s in the token's colour plus bold — for the share code
// and other hero moments that need the extra weight.
func fgBold(t colourToken) string {
	if p := sgr(t); p != "" {
		return "\x1b[1;" + p + "m"
	}
	return colorBold
}

// parseHex splits "#rrggbb" into decimal channel strings.
func parseHex(hex string) (r, g, b string) {
	ri, gi, bi := parseHexInts(hex)
	return itoa(ri), itoa(gi), itoa(bi)
}

// parseHexInts is parseHex's numeric core. Tokens are compile-time data,
// so an error path is unreachable; on malformed input it degrades to
// mid-grey rather than emitting a broken escape.
func parseHexInts(hex string) (r, g, b int) {
	if len(hex) == 7 && hex[0] == '#' {
		var v [3]int
		ok := true
		for i := range v {
			hi, hok := hexVal(hex[2*i+1])
			lo, lok := hexVal(hex[2*i+2])
			if !hok || !lok {
				ok = false
				break
			}
			v[i] = hi*16 + lo
		}
		if ok {
			return v[0], v[1], v[2]
		}
	}
	return 128, 128, 128
}

func hexVal(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var buf [3]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}

var (
	colorMu      sync.Mutex
	colorChecked bool
	colorAllow   bool

	ansiOnce sync.Once
	ansiOK   bool
)

// renderTTY reports whether w is a terminal that can interpret ANSI
// escapes (color, cursor movement). On Windows the first call flips the
// console into VT mode; when that fails (legacy conhost) every renderer
// degrades to its non-TTY output instead of printing escape garbage.
// TERM=dumb gets the same degradation — those terminals are TTYs but
// can't interpret escapes.
func renderTTY(w io.Writer) bool {
	if os.Getenv("TERM") == "dumb" || Plain() {
		return false
	}
	ansiOnce.Do(func() { ansiOK = enableANSI() })
	return ansiOK && IsTTY(w)
}

// Plain reports plain mode: FSEND_PLAIN set (non-empty, not "0"/"false"),
// or a locale that isn't UTF-8. Every renderer then takes its pipe form —
// whole lines, no redraws, ASCII glyphs, no colour — which is what screen
// readers and legacy consoles need. Read per call so tests can flip it.
func Plain() bool {
	if v := os.Getenv("FSEND_PLAIN"); v != "" && v != "0" && v != "false" {
		return true
	}
	return !utf8Locale()
}

// utf8Locale reports whether the locale's charset is UTF-8, by POSIX
// precedence (LC_ALL, LC_CTYPE, LANG). Unset everywhere counts as UTF-8:
// that is how ssh and launchd sessions often arrive at a UTF-8 terminal.
// Windows consoles don't use these variables.
func utf8Locale() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); v != "" {
			v = strings.ToLower(strings.ReplaceAll(v, "-", ""))
			return strings.Contains(v, "utf8")
		}
	}
	return true
}

// colorEnabled reports whether we should emit ANSI colour escapes.
// Rules (de-facto standard):
//
//   - NO_COLOR set (any value) → disabled
//   - FORCE_COLOR set (any non-empty, non-"0") → enabled, even on non-TTYs
//   - otherwise: enabled iff stderr is a TTY
//
// FORCE_COLOR affects colours only: glyph/spinner selection still keys
// off the writer's TTY state, so pipes always get the ASCII fallbacks.
// Cached after first check to avoid re-stat'ing stderr on every line.
func colorEnabled() bool {
	colorMu.Lock()
	defer colorMu.Unlock()
	if colorChecked {
		return colorAllow
	}
	colorChecked = true
	if on, ok := colorEnvOverride(); ok {
		colorAllow = on
		return colorAllow
	}
	colorAllow = renderTTY(os.Stderr)
	return colorAllow
}

// colorEnvOverride applies the de-facto standard env conventions.
// NO_COLOR (https://no-color.org) is "honoured" when set to any
// non-empty value → forced off; FORCE_COLOR (non-empty, not "0" or
// "false") → forced on, even on non-TTYs. ok=false means neither var
// is set — fall back to the target writer's TTY state.
func colorEnvOverride() (on, ok bool) {
	if v := os.Getenv("NO_COLOR"); v != "" {
		return false, true
	}
	if v := os.Getenv("FORCE_COLOR"); v != "" && v != "0" && v != "false" {
		return true, true
	}
	return false, false
}

// ColorFor reports whether ANSI colour should be emitted on w. Same
// NO_COLOR / FORCE_COLOR rules as colorEnabled, but keyed to w's TTY
// state instead of stderr's — for the few surfaces that write to
// stdout (--help) rather than the stderr UX stream. Not cached: callers
// are one-shot renders, not per-line emitters.
func ColorFor(w io.Writer) bool {
	if on, ok := colorEnvOverride(); ok {
		return on
	}
	return renderTTY(w)
}

// resetColorForTesting is exposed so unit tests can flip env vars
// between cases without bleeding cached state.
func resetColorForTesting() {
	colorMu.Lock()
	defer colorMu.Unlock()
	colorChecked = false
	colorAllow = false
}

// resetThemeForTesting clears the cached background/truecolour detection
// so tests can flip FSEND_THEME / COLORFGBG / COLORTERM between cases.
func resetThemeForTesting() {
	bgOnce = sync.Once{}
	bgTheme = themeUnknown
	tcOnce = sync.Once{}
	tcOK = false
}

// Compile-time assertion that os.Stderr satisfies the writer we expect.
var _ io.Writer = os.Stderr
