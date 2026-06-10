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
	utf8, ascii, color := glyphForKind(kind)
	if !IsTTY(os.Stderr) {
		return ascii
	}
	if colorEnabled() {
		return color + utf8 + colorReset
	}
	return utf8
}

// Check returns the success glyph (✓ or [OK]).
func Check() string { return Marker(gCheck) }

// Cross returns the failure glyph (✗ or [FAIL]).
func Cross() string { return Marker(gCross) }

// Warn returns the warning glyph (⚠ or [WARN]).
func Warn() string { return Marker(gWarn) }

// Info returns the informational glyph (ℹ or [INFO]).
func Info() string { return Marker(gInfo) }

// Retry returns the retry glyph (↻ or [RETRY]).
func Retry() string { return Marker(gRetry) }

func glyphForKind(k glyphKind) (utf8, ascii, color string) {
	switch k {
	case gCheck:
		return "✓", "[OK]", colorGreen
	case gCross:
		return "✗", "[FAIL]", colorRed
	case gWarn:
		return "⚠", "[!]", colorYellow
	case gInfo:
		// Cyan reads as "neutral status update" — distinct from green
		// (success) and yellow (warning). Dim looks like noise.
		return "ℹ", "[i]", colorCyan
	case gRetry:
		// Yellow signals "in-flight recovery" — same family as warn,
		// brighter than dim so the retry line catches the eye.
		return "⟳", "[~]", colorYellow
	case gSpin:
		// The animated spinner glyph (Spinner type) carries its own
		// rendering; this static fallback is only used in the rare
		// non-animated paths (e.g. tests). Cyan to match Info.
		return "…", "[*]", colorCyan
	}
	return "", "", ""
}

// IsTTY reports whether w refers to a terminal. Mirrors the helper that
// already lives in this package; kept here so glyphs can be used without
// importing the progress-bar code.
//
// Note: the canonical implementation is in uxlog.go; this file uses the
// same name so call sites have one obvious helper.

// ---------------------------------------------------------------------
// Colour handling
// ---------------------------------------------------------------------

const (
	colorReset    = "\x1b[0m"
	colorBold     = "\x1b[1m"
	colorRed      = "\x1b[31m"
	colorGreen    = "\x1b[32m"
	colorYellow   = "\x1b[33m"
	colorCyan     = "\x1b[36m"
	colorBoldCyan = "\x1b[1;36m"
	colorDim      = "\x1b[2m"
)

var (
	colorMu      sync.Mutex
	colorChecked bool
	colorAllow   bool
)

// colorEnabled reports whether we should emit ANSI colour escapes.
// Rules (de-facto standard):
//
//   - NO_COLOR set (any value) → disabled
//   - FORCE_COLOR set (any non-empty, non-"0") → enabled, even on non-TTYs
//   - otherwise: enabled iff stderr is a TTY
//
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
	colorAllow = IsTTY(os.Stderr)
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
	return IsTTY(w)
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
