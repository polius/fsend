package uxlog

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"
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
	mu   sync.Mutex
	msg  string
	w    io.Writer
	tty  bool
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

// spinnerFrames is the braille rotation used on TTYs. 10 frames at 10 Hz
// gives a 1 s rotation period, which reads as "alive" without being noisy.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinnerInterval matches the progress-bar refresh rate (≥10 Hz when
// active) so both visuals stay in sync on the same terminal.
const spinnerInterval = 100 * time.Millisecond

// StartSpinner begins drawing an animated spinner to stderr with the given
// message. The returned Spinner must be Stop'd before the caller writes
// anything else to stderr, otherwise the spinner's redraw will fight the
// caller's output.
//
// On non-TTY stderr a single static "[*] msg" line is printed instead —
// log files should record the wait happened. Quiet mode is the caller's
// concern (don't start a spinner); the package consults no flags.
func StartSpinner(msg string) *Spinner {
	s := &Spinner{
		msg:  msg,
		w:    os.Stderr,
		tty:  renderTTY(os.Stderr),
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if !s.tty {
		// Non-TTY: print one static "[*] msg" line and we're done.
		// Stop will be a no-op. A stderr write failure is non-actionable
		// from a UX helper, so the error is intentionally ignored.
		_, _ = fmt.Fprintln(s.w, Marker(gSpin), msg)
		close(s.done)
		return s
	}
	go s.run()
	return s
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
// The frame glyph is rendered in cyan to match the Info / static-Spin
// glyphs; on a dark background it reads as "active wait" without
// shouting like a yellow warn would.
func (s *Spinner) draw(frame string) {
	s.mu.Lock()
	msg := s.msg
	s.mu.Unlock()
	// Stderr write failures inside the UX layer are non-actionable —
	// the caller has bigger problems than an unrendered spinner.
	if colorEnabled() {
		_, _ = fmt.Fprintf(s.w, "\r\x1b[2K%s%s%s %s", colorCyan, frame, colorReset, msg)
	} else {
		_, _ = fmt.Fprintf(s.w, "\r\x1b[2K%s %s", frame, msg)
	}
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
