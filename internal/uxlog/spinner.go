package uxlog

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Spinner draws an animated single-line status to stderr until Stop is
// called. On non-TTYs it degrades to a single static line printed once;
// no animation, no cursor manipulation.
//
// Use it for waits with no determinate progress: pairing, mDNS lookup,
// pairing-server polling. For known-byte-count progress, use
// Progress instead.
//
// Typical lifecycle:
//
//	sp := uxlog.StartSpinner("Looking for sender on local network…")
//	doWork()
//	sp.Stop()
//	fmt.Fprintln(os.Stderr, uxlog.Check(), "Found sender on local network")
//
// Stop clears the spinner line so the next stderr write starts at column 0.
// On non-TTY the static line stays in place — Stop is a no-op visually.
type Spinner struct {
	mu    sync.Mutex
	msg   string
	hint  string // static trailer after the elapsed chip ("Ctrl-C to cancel")
	w     io.Writer
	tty   bool
	start time.Time // spinner life; rendered as an "· 45s" chip once ≥1s
	stop  chan struct{}
	done  chan struct{}
	once  sync.Once
}

// spinnerFrames is the braille rotation used on TTYs. 10 frames at 10 Hz
// gives a 1 s rotation period, which reads as "alive" without being noisy.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// HintCancel is the trailer long waits advertise so the escape hatch is
// never hidden: pass it as StartSpinner's hint.
const HintCancel = "Ctrl-C to cancel"

// spinnerInterval matches the progress-bar refresh rate (≥10 Hz when
// active) so both visuals stay in sync on the same terminal.
const spinnerInterval = 100 * time.Millisecond

// StartSpinner begins drawing an animated spinner to stderr with the given
// message. An optional hint renders as a static trailer after the elapsed
// chip — "⠋ Waiting for receiver  ·  4s  ·  Ctrl-C to cancel" — so a long
// wait always advertises its escape hatch. The returned Spinner must be
// Stop'd before the caller writes anything else to stderr, otherwise the
// spinner's redraw will fight the caller's output.
//
// On non-TTY stderr a single static "[*] msg" line is printed instead —
// log files should record the wait happened. Quiet mode is the caller's
// concern (don't start a spinner); the package consults no flags.
func StartSpinner(msg string, hint ...string) *Spinner {
	s := &Spinner{
		msg:   msg,
		w:     os.Stderr,
		tty:   renderTTY(os.Stderr),
		start: time.Now(),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	if len(hint) > 0 {
		s.hint = hint[0]
	}
	if !s.tty {
		// Non-TTY: print one static "[*] msg" line and we're done. No
		// hint: nobody at a log file can press Ctrl-C. Stop will be a
		// no-op. A stderr write failure is non-actionable from a UX
		// helper, so the error is intentionally ignored.
		_, _ = fmt.Fprintln(s.w, Marker(gSpin), s.msg)
		close(s.done)
		return s
	}
	setActiveSpinner(s)
	go s.run()
	return s
}

// ttyMu serialises whole-line writes against the live spinner's redraws,
// so a notice printed mid-wait (Ctrl-C's "Cancelling…") never lands glued
// to the end of a spinner frame.
var (
	ttyMu      sync.Mutex
	liveSpin   *Spinner
	liveSpinMu sync.Mutex
)

func setActiveSpinner(s *Spinner) {
	liveSpinMu.Lock()
	liveSpin = s
	liveSpinMu.Unlock()
}

func clearActiveSpinner(s *Spinner) {
	liveSpinMu.Lock()
	if liveSpin == s {
		liveSpin = nil
	}
	liveSpinMu.Unlock()
}

func activeSpinner() *Spinner {
	liveSpinMu.Lock()
	defer liveSpinMu.Unlock()
	return liveSpin
}

func (s *Spinner) run() {
	defer close(s.done)
	t := time.NewTicker(spinnerInterval)
	defer t.Stop()
	i := 0
	for {
		s.draw(spinnerFrames[i%len(spinnerFrames)])
		i++
		select {
		case <-s.stop:
			s.clear()
			return
		case <-t.C:
		}
	}
}

// draw writes one frame in-place using carriage return + clear-to-end.
// We use \x1b[2K (erase entire line) rather than \x1b[K (erase to end)
// because a previous longer message may have left trailing characters.
//
// The frame glyph is rendered in the brand orange: the spinner is the
// "act now / pay attention" element par excellence, so it carries the
// same accent as the share code and the code box — distinct from cyan
// (informational) and green (reassurance).
func (s *Spinner) draw(frame string) {
	s.mu.Lock()
	msg := s.msg
	hint := s.hint
	s.mu.Unlock()
	// Elapsed chip: answers "alive or hung?" while a wait stretches on.
	// Hidden for the first second so a quick pair doesn't flash "0 s";
	// after a minute the duration formatter switches to "1 min 05 s".
	// Plain default foreground — readable everywhere, quiet by position.
	elapsed := ""
	if d := time.Since(s.start); d >= time.Second {
		elapsed = spinnerElapsed(d)
	}
	// Clip to the live width every frame: a line that wraps turns each
	// \r redraw into a new row, flooding the screen ten times a second.
	text := fitSpinner(msg, elapsed, hint, terminalWidth(80))
	// Stderr write failures inside the UX layer are non-actionable —
	// the caller has bigger problems than an unrendered spinner.
	ttyMu.Lock()
	defer ttyMu.Unlock()
	if c := fg(tokenAccent); colorEnabled() && c != "" {
		_, _ = fmt.Fprintf(s.w, "\r\x1b[2K%s%s%s %s", c, frame, colorReset, text)
	} else {
		_, _ = fmt.Fprintf(s.w, "\r\x1b[2K%s %s", frame, text)
	}
}

// spinnerSep joins the spinner's message, elapsed time and hint.
const spinnerSep = "  ·  "

// fitSpinner lays out the text after the spinner frame so the whole line
// stays inside width-1 columns (the last column would trigger an
// auto-wrap on some terminals). What gives way, in order: the hint, then
// the message's tail (cut with an ellipsis); the elapsed time stays, since
// it is the "still alive?" answer. Columns are counted in runes.
func fitSpinner(msg, elapsed, hint string, width int) string {
	avail := width - 1 - 2 // frame glyph and its space
	n := utf8.RuneCountInString
	tail := ""
	if elapsed != "" {
		tail = spinnerSep + elapsed
	}
	if hint != "" {
		if full := msg + tail + spinnerSep + hint; n(full) <= avail {
			return full
		}
	}
	if n(msg+tail) <= avail {
		return msg + tail
	}
	budget := avail - n(tail)
	if budget < 6 { // too cramped for message and time: keep the message
		tail, budget = "", avail
	}
	return clipRunes(msg, budget) + tail
}

// clipRunes cuts s to at most max runes, ending in "…" when cut.
func clipRunes(s string, max int) string {
	r := []rune(s)
	switch {
	case len(r) <= max:
		return s
	case max <= 0:
		return ""
	case max == 1:
		return "…"
	}
	return strings.TrimRight(string(r[:max-1]), " ") + "…"
}

// Wrap breaks text at spaces so no line passes the terminal's width-1
// columns, indenting every line by indent spaces. For static lines (the
// tagline under the share code) that would otherwise wrap mid-word. Off a
// terminal the fallback width is wide enough that text stays one line.
func Wrap(text string, indent int) string {
	width := terminalWidth(1 << 16)
	avail := max(20, width-1-indent)
	pad := strings.Repeat(" ", indent)
	var b strings.Builder
	line := 0
	for i, w := range strings.Fields(text) {
		wl := utf8.RuneCountInString(w)
		switch {
		case i == 0:
			b.WriteString(pad)
		case line+1+wl > avail:
			b.WriteString("\n" + pad)
			line = 0
		default:
			b.WriteByte(' ')
			line++
		}
		b.WriteString(w)
		line += wl
	}
	return b.String()
}

// spinnerElapsed renders a spinner's running age in compact wall-clock
// form. Seconds for the first minute, then HumanDuration's m/s form.
func spinnerElapsed(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d s", int(d.Seconds()))
	}
	return HumanDuration(d)
}

// SetMessage swaps the spinner text; the next frame draws it. On non-TTY,
// where the original message was already printed as a static line, the new
// message is printed the same way so log readers see the state change.
// Safe on a nil receiver.
func (s *Spinner) SetMessage(msg string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	changed := msg != s.msg
	s.msg = msg
	s.mu.Unlock()
	if !s.tty && changed {
		_, _ = fmt.Fprintln(s.w, Marker(gSpin), msg)
	}
}

// clear wipes the spinner line and leaves the cursor at column 0 so the
// caller's next write starts on a clean line.
func (s *Spinner) clear() {
	ttyMu.Lock()
	defer ttyMu.Unlock()
	clearActiveSpinner(s)
	_, _ = fmt.Fprint(s.w, "\r\x1b[2K")
}

// Stop halts the animation and clears the line on TTYs. On non-TTYs it
// is a no-op (the single static line stays put). Safe to call multiple
// times; safe to call on a nil receiver.
func (s *Spinner) Stop() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		close(s.stop)
	})
	<-s.done
}
