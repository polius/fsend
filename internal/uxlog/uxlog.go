// Package uxlog renders the user-facing terminal UX defined in
// PROJECT_SPEC.md "Send-side terminal UX" + "Receive-side terminal UX",
// and docs/ux/failure-states.md.
//
// The CLI layer (cmd/fsend) owns the artifact/status block printing.
// This package owns the mid-transfer progress bar and the optional
// spinner — anything that needs cursor management or live updates.
//
// All output goes to stderr (per design rule "Stderr for everything
// visual"). When stderr is not a TTY (piping, CI logs), the renderer
// degrades to plain-text periodic updates with no cursor manipulation.
//
// Quiet mode is handled by callers: they simply do not construct a
// Progress instance. The package itself does not consult any flag — it
// renders whatever it's told to render.
package uxlog

import (
	"io"
	"os"
	"time"

	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
)

// Progress wraps an mpb.Progress + a single bar for the active file
// (single-file mode) or the overall transfer (multi-file/directory mode).
//
// For v0.1.0 we render one bar regardless of mode: it tracks total bytes
// across all files. Per-file bars (the directory-mode display in the spec)
// are deferred — a single accurate progress bar is more useful than two
// half-broken ones at this stage.
type Progress struct {
	mp  *mpb.Progress
	bar *mpb.Bar
	tty bool
	w   io.Writer
}

// New constructs a Progress that writes to stderr (the only sink the spec
// allows for visual output).
//
// totalBytes is the entire transfer's byte count. It must be known up
// front; for stdin/text transfers we pass 0 and the bar renders without
// a percentage.
func New(totalBytes int64) *Progress {
	tty := IsTTY(os.Stderr)
	p := &Progress{tty: tty, w: os.Stderr}

	// mpb's default output is stderr; we still set it explicitly for
	// clarity.
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

	decorators := []decor.Decorator{
		decor.Name("  "),
		decor.CountersKibiByte("% .2f / % .2f"),
		decor.Name("   "),
		decor.AverageSpeed(decor.SizeB1024(0), "% .2f"),
	}
	if totalBytes > 0 {
		// Known size → append ETA + percentage.
		decorators = append(decorators,
			decor.Name("   ETA "),
			decor.AverageETA(decor.ET_STYLE_GO),
		)
	}

	style := mpb.BarStyle()
	if !tty {
		// ASCII-only style for pipes / non-TTYs.
		style = style.Lbound("[").Rbound("]").Filler("#").Tip("#").Padding(" ")
	}

	p.bar = p.mp.New(totalBytes,
		style,
		mpb.PrependDecorators(
			decor.OnComplete(decor.Percentage(decor.WC{W: 5}), " done"),
		),
		mpb.AppendDecorators(decorators...),
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
// (" done") instead of being silently aborted:
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
