package uxlog

import (
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Glyph kinds for status lines. Each maps to a unicode + ASCII fallback
// pair and a colour. Centralising them here keeps every CLI surface
// (send, receive, connect, uninstall, errors) consistent.
type glyphKind int

const (
	gCheck glyphKind = iota // ✓ / [OK]
	gCross                  // ✗ / FAIL
	gWarn                   // ⚠ / [!]
	gInfo                   // ℹ / [i]
	gStep                   // › / >  (narration marker, as the install scripts print)
	gRetry                  // ⟳ / [~]
	gSpin                   // … / [*]  ("waiting" — non-animated; deliberately not a
	//                                   single-frame braille spinner)
)

// Marker returns the leading status glyph appropriate for stderr's TTY
// state. Caller appends the rest of the line.
//
// When stderr is a real terminal we render the unicode glyph and (when
// allowed) apply colour. When stderr is a pipe/file we render the ASCII
// fallback with no colour so log files stay readable.
func Marker(kind glyphKind) string {
	utf8, ascii, token := glyphForKind(kind)
	if !renderTTY(os.Stderr) {
		return ascii
	}
	if colorEnabled() && token.dark != "" {
		return fg(token) + utf8 + colorReset
	}
	return utf8
}

// Check returns the success glyph (✓ or [OK]).
func Check() string { return Marker(gCheck) }

// Cross returns the failure glyph (✗ or [FAIL]).
func Cross() string { return Marker(gCross) }

// Warn returns the warning glyph (⚠ or [!]).
func Warn() string { return Marker(gWarn) }

// Info returns the informational glyph (ℹ or [i]).
func Info() string { return Marker(gInfo) }

// Step returns the step glyph (› or >) — the marker the install scripts
// (scripts/install.sh, install.ps1) print before each action they take.
// fsend uses it for its own narration ahead of a re-run installer
// (--update) so the two outputs read as one continuous block.
func Step() string { return Marker(gStep) }

// Retry returns the retry glyph (⟳ or [~]).
func Retry() string { return Marker(gRetry) }

// PasswordChip renders the "password required" artifact chip, degrading
// to plain text on pipes/files like every other glyph here. ⚠ (warning)
// keeps the chip inside the app's glyph family — an emoji would be the
// odd one out and renders double-width in some terminals.
func PasswordChip() string {
	if !renderTTY(os.Stderr) {
		return "[password required]"
	}
	if colorEnabled() {
		return fg(tokenWarning) + "⚠" + colorReset + " password required"
	}
	return "⚠ password required"
}

func glyphForKind(k glyphKind) (utf8, ascii string, token colourToken) {
	switch k {
	case gCheck:
		return "✓", "[OK]", tokenSuccess
	case gCross:
		return "✗", "[FAIL]", tokenError
	case gWarn:
		return "⚠", "[!]", tokenWarning
	case gInfo:
		// Cyan reads as "neutral status update" — distinct from green
		// (success) and yellow (warning).
		return "ℹ", "[i]", tokenInfo
	case gStep:
		// Default foreground: narration, not information. The › is the
		// same glyph the install scripts print for every step, so
		// --update's lines and the installer output that follows them
		// match. Reserving cyan for the ℹ glyph, --help syntax and links
		// keeps that hue for the things worth noticing — a narration
		// marker competing in cyan only added noise.
		return "›", ">", colourToken{}
	case gRetry:
		// Yellow signals "in-flight recovery" — same family as warn,
		// so the retry line catches the eye without crying error.
		return "⟳", "[~]", tokenWarning
	case gSpin:
		// The animated spinner glyph (Spinner type) carries its own
		// rendering; this static fallback is only used in the rare
		// non-animated paths (e.g. tests). Info cyan to match ℹ.
		return "…", "[*]", tokenInfo
	}
	return "", "", colourToken{}
}

// ---------------------------------------------------------------------
// Colour handling
// ---------------------------------------------------------------------

// colourToken is one semantic colour role, holding a truecolour hex for
// each background the terminal might have. The roles mirror the theme
// keys opencode's theme system defines (primary / accent / success /
// warning / error / info), so the CLI vocabulary and the TUI vocabulary
// stay parallel:
//
//	accent   — act: the share code, code box, spinner, direction arrows
//	primary  — decide: question lines, overwrite prompts, password prompts
//	success  — reassurance and file references (✓, paths — the green the
//	           eye learns to read as "a file on disk")
//	warning  — caution (⚠, "differs", stalled bar)
//	error    — failure (✗)
//	info     — neutral status (ℹ, --help flags, links)
//
// dark is the pastel family the roles were tuned for (Tokyo Night): it
// glows on a near-black terminal but drops to ~2:1 on a light one. light
// is the same hue family darkened to clear WCAG AA on white, so a light
// terminal gets the same vocabulary at a readable contrast. The zero
// token (both fields empty) means "no colour" — the default foreground.
//
// Rendering uses truecolour when the terminal reports it
// (COLORTERM=truecolor/24bit), else the nearest 256-colour approximation
// of whichever hex applies.
type colourToken struct {
	dark  string // "#rrggbb" for a dark terminal background
	light string // "#rrggbb" for a light terminal background
}

var (
	tokenAccent  = colourToken{"#ff9e64", "#b15c00"}
	tokenPrimary = colourToken{"#bb9af7", "#7847bd"}
	tokenSuccess = colourToken{"#9ece6a", "#587539"}
	tokenWarning = colourToken{"#e0af68", "#8c6c3e"}
	tokenError   = colourToken{"#f7768e", "#b3261e"}
	tokenInfo    = colourToken{"#7dcfff", "#007197"}
)

const (
	colorReset = "\x1b[0m"
	colorBold  = "\x1b[1m"
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

// sgr returns the SGR foreground parameters for the token at the
// terminal's best fidelity: "38;2;r;g;b" under truecolour, else
// "38;5;<idx>" using the nearest 256-colour cell.
func sgr(t colourToken) string {
	hex := t.hex()
	if truecolorEnabled() {
		r, g, b := parseHex(hex)
		return "38;2;" + r + ";" + g + ";" + b
	}
	return "38;5;" + nearest256(hex)
}

// hex is the token's truecolour for the terminal's background.
func (t colourToken) hex() string {
	if darkBackground() {
		return t.dark
	}
	return t.light
}

// darkBackground reports whether the terminal's background is dark, so
// the palette picks the sibling tuned to contrast against it. Dark is the
// default: the roles were designed around the dark form, and an
// unset/unknown terminal is overwhelmingly a dark theme.
//
//  1. FSEND_THEME=light|dark — explicit, documented override.
//  2. COLORFGBG — the de-facto var iTerm2/Konsole/rxvt export as
//     "<fg>;<bg>"; the last field is the background's ANSI colour index.
//     Index 7 (white) and 9..15 (bright) are the light end.
//  3. dark.
var (
	bgOnce sync.Once
	bgDark bool
)

func darkBackground() bool {
	bgOnce.Do(func() {
		switch {
		case strings.EqualFold(os.Getenv("FSEND_THEME"), "light"):
			bgDark = false
		case strings.EqualFold(os.Getenv("FSEND_THEME"), "dark"):
			bgDark = true
		default:
			bgDark = true // unknown/unset → the designed-for default
			if c := os.Getenv("COLORFGBG"); c != "" {
				if i := strings.LastIndexByte(c, ';'); i >= 0 {
					if bg, err := strconv.Atoi(c[i+1:]); err == nil {
						bgDark = !(bg == 7 || bg >= 9)
					}
				}
			}
		}
	})
	return bgDark
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
// uses the gated colour helpers in format.go).
func fg(t colourToken) string { return "\x1b[" + sgr(t) + "m" }

// fgBold renders s in the token's colour plus bold — for the share code
// and other hero moments that need the extra weight.
func fgBold(t colourToken) string { return "\x1b[" + "1;" + sgr(t) + "m" }

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
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	ansiOnce.Do(func() { ansiOK = enableANSI() })
	return ansiOK && IsTTY(w)
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
	bgDark = false
	tcOnce = sync.Once{}
	tcOK = false
}

// Compile-time assertion that os.Stderr satisfies the writer we expect.
var _ io.Writer = os.Stderr
