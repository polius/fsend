package uxlog

import (
	"fmt"
	"time"
)

// HumanBytes renders a byte count in compact human-readable form.
// Sub-KiB values render as whole bytes ("169 B"); larger values use one
// decimal of precision against 1024-based units ("1.5 MB"). The unit
// suffix is always uppercase "B" (bytes), never lowercase "b" (bits).
//
// Lives in uxlog (rather than cmd/fsend) so the progress-bar decorators
// can share the exact same formatter the summary lines use — there's no
// way for the two to drift apart and surprise the user.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	v := float64(n) / float64(div)
	// Round values drop the decimal: "100 MB", not "100.0 MB".
	if v == float64(int64(v)) {
		return fmt.Sprintf("%d %cB", int64(v), "KMGTPE"[exp])
	}
	return fmt.Sprintf("%.1f %cB", v, "KMGTPE"[exp])
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
// The thresholds are tuned so a one-second 1 MiB transfer reports a
// rate (genuinely useful), but a one-second 169 B transfer does not
// (the number would be dominated by setup time).
func HumanRate(bytes int64, elapsed time.Duration) string {
	if elapsed < 100*time.Millisecond || bytes <= 0 {
		return ""
	}
	// Sub-MiB transfers complete in less time than the connection setup
	// takes, so the computed rate is just noise. Skip.
	const rateNoiseFloor = 1 << 20
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

// Code renders a share code in bold + cyan so it stands out as the one
// thing the user is about to type or dictate. Degrades to plain text
// when color is disabled or stderr is not a TTY.
func Code(c string) string {
	if !colorEnabled() {
		return c
	}
	return colorBoldCyan + c + colorReset
}

// Dim wraps s in the ANSI dim escape (or returns it unchanged when
// color is off). Useful for secondary metadata in artifact lines.
func Dim(s string) string {
	if !colorEnabled() {
		return s
	}
	return colorDim + s + colorReset
}

// Bold wraps s in the ANSI bold escape, unconditionally. Unlike Dim and
// Code it carries no colour gate of its own: it decorates --help, which
// cobra writes to stdout, so the caller must gate on stdout's state via
// ColorFor — the stderr-keyed colorEnabled would be the wrong check.
func Bold(s string) string {
	return colorBold + s + colorReset
}
