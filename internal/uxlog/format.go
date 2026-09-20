package uxlog

import (
	"fmt"
	"strings"
	"time"
)

// HumanBytes renders a byte count in compact human-readable form.
// Sub-KB values render as whole bytes ("169 B"); larger values use one
// decimal of precision against decimal (1000-based) units ("1.5 MB") —
// matching what Finder/Explorer report for the same file. The unit
// suffix is always uppercase "B" (bytes), never lowercase "b" (bits).
//
// Lives in uxlog (rather than cmd/fsend) so the progress-bar decorators
// can share the exact same formatter the summary lines use — there's no
// way for the two to drift apart and surprise the user.
func HumanBytes(n int64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	// Values that *display* round drop the decimal — 300006 B must read
	// "300 KB", not "300.0 KB" (which sat inconsistently next to an exact
	// 300000 B rendering as "300 KB").
	s := strings.TrimSuffix(fmt.Sprintf("%.1f", v), ".0")
	return fmt.Sprintf("%s %cB", s, "KMGTPE"[exp])
}

// HumanDuration renders elapsed in compact form. Sub-second durations
// show with milliseconds; longer durations switch to seconds with one
// decimal, then minutes-and-seconds, then hours-minutes-seconds.
func HumanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
}

// HumanRate renders a bytes-per-second figure in compact form. Returns
// "" when the figure would be meaningless (zero bytes, an elapsed window
// too small to measure, or a total small enough that handshake noise
// dominates the rate). Callers can omit a trailing "(<rate>)" clause
// cleanly instead of printing "4.2 GB/s" for a 12 KB transfer.
//
// The thresholds are tuned so a one-second 1 MB transfer reports a
// rate (genuinely useful), but a one-second 169 B transfer does not
// (the number would be dominated by setup time).
func HumanRate(bytes int64, elapsed time.Duration) string {
	if elapsed < 100*time.Millisecond || bytes <= 0 {
		return ""
	}
	// Sub-MB transfers complete in less time than the connection setup
	// takes, so the computed rate is just noise. Skip. Matches the
	// HumanBytes "1 MB" boundary so any size displayed in MB has a rate.
	const rateNoiseFloor = 1000 * 1000
	if bytes < rateNoiseFloor {
		return ""
	}
	rate := float64(bytes) / elapsed.Seconds()
	return HumanBytes(int64(rate)) + "/s"
}

// CountNoun renders "<n> <noun>" with naive pluralisation ("1 file",
// "3 files").
func CountNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Code renders a share code in bold + accent orange — the hero moment —
// so it stands out as the one thing the user is about to type or
// dictate. Degrades to plain text when color is disabled or stderr is
// not a TTY.
func Code(c string) string {
	if !colorEnabled() {
		return c
	}
	return fgBold(tokenAccent) + c + colorReset
}

// There is deliberately no "dim"/muted renderer here. Secondary text
// renders in the terminal's default foreground: that is the one colour
// guaranteed readable on the user's theme, and hierarchy comes from
// emphasising the primary elements (accent orange, primary violet,
// success green, bold) — never from darkening the rest. opencode can
// calibrate a muted tone against the known background; a portable CLI
// cannot, and every grey gamble loses on someone's palette.

// Prompt wraps s in the primary accent — the colour of a question, the
// role opencode's themes give their primary token. Used for the lines
// that ask the user to decide (accept, overwrite, password) so a scan
// of the terminal finds every point where input is wanted. Gated on
// colour.
func Prompt(s string) string {
	if !colorEnabled() {
		return s
	}
	return fg(tokenPrimary) + s + colorReset
}

// Good wraps s in the success colour — the reassurance family the ✓
// glyph leads — for positive clauses in summary lines ("2 files up to
// date"). Gated on colour.
func Good(s string) string {
	if !colorEnabled() {
		return s
	}
	return fg(tokenSuccess) + s + colorReset
}

// Path wraps s in the success colour — the file-reference accent. In
// opencode's TUI, file paths and code references render through the
// theme's green tokens (diffAdded / markdownCode / syntaxString share
// one green), which teaches the eye "green = a file on disk". fsend
// follows the same convention: listing names, saved destinations,
// conflict rows. Shares the green with Good so the association stays
// single. Gated on colour.
func Path(s string) string {
	if !colorEnabled() {
		return s
	}
	return fg(tokenSuccess) + s + colorReset
}

// Added wraps s in the diff-added colour — opencode's diffAdded role.
// Used where the UI narrates a change as old → new (the overwrite
// prompt's size delta): the incoming, about-to-be-written side.
// Gated on colour.
func Added(s string) string {
	if !colorEnabled() {
		return s
	}
	return fg(tokenSuccess) + s + colorReset
}

// Removed wraps s in the diff-removed colour — opencode's diffRemoved
// role. The counterpart of Added: the on-disk side a consented overwrite
// will replace. Gated on colour.
func Removed(s string) string {
	if !colorEnabled() {
		return s
	}
	return fg(tokenError) + s + colorReset
}

// Alert wraps s in the warning colour (the same family as the ⚠ glyph)
// so an attention-worthy inline tag — e.g. a "differs" status — stands
// out instead of receding. Plain text when colour is off.
func Alert(s string) string {
	if !colorEnabled() {
		return s
	}
	return fg(tokenWarning) + s + colorReset
}

// Accent wraps s in the info colour — shared with the ℹ glyph.
// Unconditional, like Bold: callers that need the element to disappear
// entirely on pipes gate the call on ColorFor themselves (--help flag
// names).
func Accent(s string) string {
	return fg(tokenInfo) + s + colorReset
}

// Link wraps s in the info colour for URL references (the LEARN MORE
// footer of --help) — the markdown-link role. Gated on colour.
func Link(s string) string {
	if !colorEnabled() {
		return s
	}
	return fg(tokenInfo) + s + colorReset
}

// Brand wraps s in the accent colour, unconditionally like Bold and
// Accent. The caller gates it (ColorFor) for elements that must vanish
// on pipes: direction arrows, the code box frame.
func Brand(s string) string {
	return fg(tokenAccent) + s + colorReset
}

// Bold wraps s in the ANSI bold escape, unconditionally. Unlike Prompt
// and Good it carries no colour gate of its own: it decorates --help,
// which cobra writes to stdout, so the caller must gate on stdout's
// state via ColorFor — the stderr-keyed colorEnabled would be the wrong
// check.
func Bold(s string) string {
	return colorBold + s + colorReset
}
