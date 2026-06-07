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
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

// Progress wraps an mpb.Progress + a single bar that tracks total bytes
// across the whole transfer regardless of mode (single file, multi-file,
// or tar-bundled directory).
type Progress struct {
	mp  *mpb.Progress
	bar *mpb.Bar
	tty bool
	w   io.Writer
}

// barWidth caps the progress bar at a fixed column count. 40 leaves room
// for a leading percentage block, a generous "1.5 GB / 2.3 GB" counter,
// and the rate/ETA chips without bumping past 100 columns even on narrow
// terminals. Full-width bars look "loud" — see CLI UX review notes.
const barWidth = 40

// rateThreshold is the transfer size below which rate + ETA are
// suppressed. Small transfers (sub-MiB) finish in less time than the
// PAKE handshake takes; reporting "13 B/s" for a 169 B file is just
// noise. Above 1 MiB the steady-state throughput dominates and the
// figure becomes meaningful.
const rateThreshold = 1 << 20

// New constructs a Progress that writes to stderr (the only sink the spec
// allows for visual output).
//
// totalBytes is the entire transfer's byte count. It must be known up
// front; for stdin/text transfers we pass 0 and the bar renders without
// a percentage, ETA, or rate chip.
func New(totalBytes int64) *Progress {
	tty := IsTTY(os.Stderr)
	p := &Progress{tty: tty, w: os.Stderr}

	opts := []mpb.ContainerOption{
		mpb.WithOutput(os.Stderr),
		mpb.WithRefreshRate(100 * time.Millisecond), // spec: ≥10 Hz
	}
	if !tty {
		// Pipe mode: emit periodic plain-text updates with no cursor
		// manipulation. mpb still draws bars but we let it; downstream
		// consumers see "\r"-overwritten lines that they can grep with
		// the % column.
		opts = append(opts, mpb.WithAutoRefresh())
	}
	p.mp = mpb.New(opts...)

	// Unicode bar on TTYs; ASCII fallback on pipes so log files stay
	// readable. The ━/╸/─ trio gives a calm, modern look without the
	// "=====>" telegraph aesthetic the default style carries.
	var style mpb.BarStyleComposer
	if tty {
		style = mpb.BarStyle().Lbound(" ").Rbound(" ").Filler("━").Tip("╸").Padding("─")
	} else {
		style = mpb.BarStyle().Lbound("[").Rbound("]").Filler("#").Tip(">").Padding(" ")
	}

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
		decor.OnComplete(decor.Percentage(decor.WC{W: 4}), "done"),
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
	if p == nil || p.bar == nil {
		return
	}
	p.bar.IncrInt64(n)
}

// SetTotal updates the bar's total. Useful for stdin transfers where the
// final size is only known when EOF arrives.
func (p *Progress) SetTotal(total int64, complete bool) {
	if p == nil || p.bar == nil {
		return
	}
	p.bar.SetTotal(total, complete)
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
	if p == nil || p.mp == nil {
		return
	}
	if p.bar != nil && !p.bar.Completed() {
		p.bar.Abort(false)
	}
	p.mp.Wait()
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
