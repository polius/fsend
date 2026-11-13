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

// HumanDuration renders elapsed in compact form, with a space before the
// unit like every size ("300 MB in 4.8 s"). Sub-second durations show
// milliseconds; then seconds with one decimal, whole seconds, minutes and
// seconds, and past an hour hours and minutes.
func HumanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%d ms", d.Milliseconds())
	}
	if d < 10*time.Second {
		return fmt.Sprintf("%.1f s", d.Seconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%d min %02d s", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	return fmt.Sprintf("%d h %02d min", h, m)
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
	return RateLabel(float64(bytes) / elapsed.Seconds())
}

// RateLabel renders a bytes-per-second figure with a fixed single decimal
// ("61.0 MB/s", never "61 MB/s" then "60.1 MB/s"), so a live rate keeps
// its shape while it is being read. Sizes elsewhere drop a ".0"; a rate
// changes every frame, so it must not.
func RateLabel(bytesPerSec float64) string {
	const unit = 1000
	n := int64(bytesPerSec)
	if n < unit {
		return fmt.Sprintf("%d B/s", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB/s", float64(n)/float64(div), "KMGTPE"[exp])
}

// Percent is the one rounding every percentage on screen uses — the bar,
// plain-mode lines, "stopped at 62%" and "Resuming from … (62%)" — so two
// lines about the same moment never disagree by a point. Rounds to the
// nearest whole percent, but never shows 100 before the end or 0 after
// the start.
func Percent(current, total int64) int {
	if total <= 0 || current <= 0 {
		return 0
	}
	if current >= total {
		return 100
	}
	p := int((current*200 + total) / (total * 2))
	return max(1, min(99, p))
}

// CountNoun renders "<n> <noun>" with naive pluralisation ("1 file",
// "3 files").
func CountNoun(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// Code renders a share code — or any command the user is about to run —
// in bold + accent orange, the hero treatment: the one thing on the line
// the user is about to type or dictate. Degrades to plain text when
// color is disabled or stderr is not a TTY.
func Code(c string) string {
	if !colorEnabled() {
		return c
	}
	return fgBold(tokenAccent) + c + colorReset
}

// Three levels rank everything on screen: bold for the subject of a line,
// default text for the sentence, faint (Muted) for the detail — sizes,
// counts, the route, timings, hints, rows that need no attention. Colour
// is left for what needs a decision or reports an outcome, so a screen
// needs few hues.

// Muted renders s in the faint attribute (SGR 2) for metadata. Faint is
// derived from the user's own foreground, so it stays readable on every
// theme, unlike a fixed grey. Gated on colour; plain text on pipes.
func Muted(s string) string {
	if !colorEnabled() || s == "" {
		return s
	}
	return colorFaint + s + "\x1b[22m"
}

// Subject renders the subject of a line — the file a summary is about,
// the destination a prompt names — in bold. Gated on colour.
func Subject(s string) string {
	if !colorEnabled() || s == "" {
		return s
	}
	return colorBold + s + "\x1b[22m"
}

// Sep joins the facts on a line: one separator, one space each side.
const Sep = " · "

// Join joins the non-empty parts with Sep.
func Join(parts ...string) string {
	out := parts[:0:0]
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, Sep)
}

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

// Path marks a file reference. File names render in the default
// foreground — green means "it worked" and is kept for the ✓ alone — so
// this is the identity; it stays as the one place to change if names ever
// need a treatment of their own.
func Path(s string) string { return s }

// Good marks a positive clause. Green is kept for the ✓ alone, so this
// renders default text; it remains so callers can keep marking intent.
func Good(s string) string { return s }

// Alert wraps s in the warning colour (the same family as the ! glyph)
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

// Bold wraps s in the ANSI bold escape, unconditionally. Unlike Prompt
// and Good it carries no colour gate of its own: it decorates --help,
// which cobra writes to stdout, so the caller must gate on stdout's
// state via ColorFor — the stderr-keyed colorEnabled would be the wrong
// check.
func Bold(s string) string {
	return colorBold + s + colorReset
}
