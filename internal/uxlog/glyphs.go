package uxlog

import (
	"io"
	"os"
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
	if colorEnabled() {
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

// colourToken is one semantic colour role, holding its truecolour hex
// and the 256-colour approximation used when the terminal doesn't
// advertise truecolour. The roles mirror the theme keys opencode's
// theme system defines (primary / accent / success / warning / error /
// info), so the CLI vocabulary and the TUI vocabulary stay parallel:
//
//	accent   — act: the share code, code box, spinner, direction arrows
//	primary  — decide: question lines, overwrite prompts, password prompts
//	success  — reassurance and file references (✓, paths — the green the
//	           eye learns to read as "a file on disk")
//	warning  — caution (⚠, "differs", stalled bar)
//	error    — failure (✗)
//	info     — neutral status (ℹ, --help flags)
//
// Rendering follows opencode's documented model: truecolour when the
// terminal reports it (COLORTERM=truecolor/24bit), nearest 256-colour
// approximation otherwise.
type colourToken struct {
	hex  string // "#rrggbb", used when truecolour is available
	c256 string // nearest 256-colour index, the fallback
}

var (
	tokenAccent  = colourToken{"#ff9e64", "209"}
	tokenPrimary = colourToken{"#bb9af7", "140"}
	tokenSuccess = colourToken{"#9ece6a", "114"}
	tokenWarning = colourToken{"#e0af68", "179"}
	tokenError   = colourToken{"#f7768e", "204"}
	tokenInfo    = colourToken{"#7dcfff", "117"}
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
// "38;5;<idx>".
func sgr(t colourToken) string {
	if truecolorEnabled() {
		r, g, b := parseHex(t.hex)
		return "38;2;" + r + ";" + g + ";" + b
	}
	return "38;5;" + t.c256
}

// fg renders s in the token's colour; caller wraps with colorReset (or
// uses the gated colour helpers in format.go).
func fg(t colourToken) string { return "\x1b[" + sgr(t) + "m" }

// fgBold renders s in the token's colour plus bold — for the share code
// and other hero moments that need the extra weight.
func fgBold(t colourToken) string { return "\x1b[" + "1;" + sgr(t) + "m" }

// parseHex splits "#rrggbb" into decimal channel strings. Tokens are
// compile-time data, so an error path is unreachable; on malformed input
// it degrades to mid-grey rather than emitting a broken escape.
func parseHex(hex string) (r, g, b string) {
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
			return itoa(v[0]), itoa(v[1]), itoa(v[2])
		}
	}
	return "128", "128", "128"
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

// Compile-time assertion that os.Stderr satisfies the writer we expect.
var _ io.Writer = os.Stderr
