// Package uxlog renders fsend's user-facing terminal UX.
//
// The CLI layer (cmd/fsend) owns the artifact/status block printing.
// This package owns the mid-transfer progress bar and the optional
// spinner — anything that needs cursor management or live updates.
//
// All output goes to stderr. When stderr is not a TTY (piping, CI logs),
// the renderer degrades to plain-text periodic updates with no cursor
// manipulation.
//
// Quiet mode is handled by callers: they simply do not construct a
// Progress instance. The package itself does not consult any flag — it
// renders whatever it's told to render.
package uxlog

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"golang.org/x/term"
)

// Progress wraps an mpb.Progress + a single bar that tracks total bytes
// across the whole transfer regardless of mode (single file, multi-file,
// or tar-bundled directory). On non-TTY stderr the mpb pair is nil and
// plain carries a line-oriented renderer instead.
type Progress struct {
	mp    *mpb.Progress
	bar   *mpb.Bar
	plain *plainProgress
}

// plainProgress renders progress as occasional complete lines — no
// cursor movement, no in-place redraws — so pipes and CI logs stay
// readable. mpb is unsuitable here: even its non-interactive mode
// emits cursor-up/erase escapes between frames.
type plainProgress struct {
	mu       sync.Mutex
	w        io.Writer
	total    int64
	current  int64
	complete bool
	closed   bool
	lastLine time.Time
}

// plainInterval throttles plain-mode lines. One line per second keeps
// long transfers observable without flooding logs.
const plainInterval = time.Second

func (p *plainProgress) add(n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.current += n
	if time.Since(p.lastLine) < plainInterval {
		return
	}
	p.lastLine = time.Now()
	if p.total > 0 {
		_, _ = fmt.Fprintf(p.w, "  %d%%  %s / %s\n",
			p.current*100/p.total, HumanBytes(p.current), HumanBytes(p.total))
		return
	}
	_, _ = fmt.Fprintf(p.w, "  %s\n", HumanBytes(p.current))
}

func (p *plainProgress) setTotal(total int64, complete bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.total = total
	p.complete = p.complete || complete
}

// done prints one terminal line for transfers that actually completed.
// Partial transfers print nothing — the error line that follows is the
// authoritative record. Idempotent: callers flush before the summary
// and keep a deferred call as the safety net.
func (p *plainProgress) done() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// No final "done <size>" line — the summary line that follows
	// already carries the size.
	p.closed = true
}

// barWidth caps the progress bar at a fixed column count. 40 leaves room
// for a leading percentage block, a generous "1.5 GB / 2.3 GB" counter,
// and the rate/ETA chips without bumping past 100 columns even on narrow
// terminals. Full-width bars look "loud" — see CLI UX review notes.
const barWidth = 40

// rateThreshold is the transfer size below which rate + ETA are
// suppressed. Small transfers (sub-MB) finish in less time than the
// PAKE handshake takes; reporting "13 B/s" for a 169 B file is just
// noise. Above 1 MB the steady-state throughput dominates and the
// figure becomes meaningful. Matches HumanRate's noise floor.
const rateThreshold = 1000 * 1000

// activeProgress is the bar currently drawing on the terminal, if any.
// Println routes through it so foreign lines (retry notices) land between
// refresh frames instead of inside one. There is only ever one bar at a
// time per process.
var (
	activeMu       sync.Mutex
	activeProgress *Progress
)

// Println writes a full line to stderr. While a TTY progress bar is
// live, the line is handed to mpb so it prints above the bar instead of
// colliding with the in-place redraw.
func Println(line string) {
	activeMu.Lock()
	p := activeProgress
	activeMu.Unlock()
	if p != nil && p.mp != nil {
		if _, err := fmt.Fprintln(p.mp, line); err == nil {
			return
		}
		// mpb already shut down (ErrDone) — fall through to plain stderr.
	}
	fmt.Fprintln(os.Stderr, line)
}

func setActive(p *Progress) {
	activeMu.Lock()
	activeProgress = p
	activeMu.Unlock()
}

// New constructs a Progress that writes to stderr (the only sink the spec
// allows for visual output).
//
// totalBytes is the entire transfer's byte count. It must be known up
// front; for stdin/text transfers we pass 0 and the bar renders without
// a percentage, ETA, or rate chip.
func New(totalBytes int64) *Progress {
	// Plain mode for pipes/CI, and for terminals that report a 0×0
	// window (some pty wrappers) — mpb discards every row at height 0.
	width, _, sizeErr := term.GetSize(int(os.Stderr.Fd()))
	if !renderTTY(os.Stderr) || sizeErr != nil || width <= 0 {
		return &Progress{plain: &plainProgress{
			w: os.Stderr, total: totalBytes, lastLine: time.Now(),
		}}
	}
	p := &Progress{}
	defer setActive(p)

	p.mp = mpb.New(
		mpb.WithOutput(os.Stderr),
		mpb.WithRefreshRate(100*time.Millisecond), // spec: ≥10 Hz
	)

	// The ━/╸/─ trio gives a calm, modern look without the "=====>"
	// telegraph aesthetic the default style carries.
	style := mpb.BarStyle().Lbound(" ").Rbound(" ").Filler("━").Tip("╸").Padding("─")

	// Track elapsed locally — decor.Statistics doesn't carry it, and we
	// want the rate decor to use the same "since New()" baseline the
	// summary line will use for "(<rate>)".
	start := time.Now()
	hasTotal := totalBytes > 0
	showRate := totalBytes >= rateThreshold

	// Percentage on the left. OnComplete swaps it for "done" so the bar
	// reads as terminal once the transfer is finished — no stale ETA
	// chip lingering at 0s.
	prependDecs := []decor.Decorator{
		decor.Name("  "),
		// "%d" renders "66%", matching plain mode — the default
		// "% d" pads a space before the sign ("66 %").
		decor.OnComplete(decor.NewPercentage("%d", decor.WC{W: 4}), "done"),
		decor.Name("  "),
	}
	if !hasTotal {
		// Streaming transfers (stdin) — no meaningful percentage.
		// Replace with a static dots glyph that doesn't shift width
		// when the transfer finishes (closeFn latches the total then).
		prependDecs = []decor.Decorator{
			decor.Name("  "),
			decor.OnComplete(decor.Name("... "), "done"),
			decor.Name("  "),
		}
	}

	// Counters: always render via HumanBytes so units stay uppercase
	// and small whole-byte values don't gain a "%.2f" tail.
	counters := decor.Any(func(s decor.Statistics) string {
		if s.Total <= 0 {
			return HumanBytes(s.Current)
		}
		return fmt.Sprintf("%s / %s", HumanBytes(s.Current), HumanBytes(s.Total))
	})

	appendDecs := []decor.Decorator{
		decor.Name("  "),
		counters,
	}
	if showRate {
		appendDecs = append(appendDecs,
			// Rate: hidden when the figure would be misleading (start
			// of transfer, zero elapsed, or on completion — the
			// summary line carries the final figure).
			decor.Any(func(s decor.Statistics) string {
				if s.Completed || s.Aborted || s.Current == 0 {
					return ""
				}
				elapsed := time.Since(start)
				r := HumanRate(s.Current, elapsed)
				if r == "" {
					return ""
				}
				return "  ·  " + r
			}),
			// ETA: needs a known total, non-zero progress, and at least
			// 1 s elapsed so the projection isn't dominated by handshake
			// time. Hidden on completion.
			decor.Any(func(s decor.Statistics) string {
				if s.Completed || s.Aborted || s.Total <= 0 || s.Current == 0 {
					return ""
				}
				elapsed := time.Since(start)
				if elapsed < time.Second {
					return ""
				}
				rate := float64(s.Current) / elapsed.Seconds()
				if rate <= 0 {
					return ""
				}
				remainingSecs := float64(s.Total-s.Current) / rate
				if remainingSecs <= 0 {
					return ""
				}
				return "  ·  ETA " + HumanDuration(time.Duration(remainingSecs*float64(time.Second)))
			}),
		)
	}

	p.bar = p.mp.New(totalBytes,
		style,
		mpb.BarWidth(barWidth),
		mpb.PrependDecorators(prependDecs...),
		mpb.AppendDecorators(appendDecs...),
	)
	return p
}

// Add increments the bar by n bytes.
//
// Safe to call from multiple goroutines (mpb's bar is internally
// synchronized).
func (p *Progress) Add(n int64) {
	if p == nil {
		return
	}
	if p.plain != nil {
		p.plain.add(n)
		return
	}
	if p.bar != nil {
		p.bar.IncrInt64(n)
	}
}

// SetTotal updates the bar's total. Useful for stdin transfers where the
// final size is only known when EOF arrives.
func (p *Progress) SetTotal(total int64, complete bool) {
	if p == nil {
		return
	}
	if p.plain != nil {
		p.plain.setTotal(total, complete)
		return
	}
	if p.bar != nil {
		p.bar.SetTotal(total, complete)
	}
}

// Done marks the bar complete and waits for mpb to flush. Always call
// this before the caller exits.
//
// Two paths so the success case still renders the OnComplete suffix
// ("done") instead of being silently aborted:
//
//   - Bar already at 100% (Completed reports true): the final Add()
//     already triggered OnComplete; just Wait for mpb to flush.
//   - Bar below 100% (aborted mid-transfer): Abort(false) so mpb
//     releases the bar and Wait returns instead of hanging forever
//     waiting for the bar to reach its declared total. We deliberately
//     don't SetTotal here — that would render a misleading "done" on
//     a partial bar.
func (p *Progress) Done() {
	if p == nil {
		return
	}
	if p.plain != nil {
		p.plain.done()
		return
	}
	if p.mp == nil {
		return
	}
	if p.bar != nil && !p.bar.Completed() {
		p.bar.Abort(false)
	}
	p.mp.Wait()
	setActive(nil)
}

// IsTTY reports whether w refers to a terminal.
func IsTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
