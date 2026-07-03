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
	"math"
	"os"
	"sync"
	"sync/atomic"
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
	label atomic.Value // string: current-file chip (see SetLabel)
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
	start    time.Time
	label    string // current-file chip, "" when unset
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
		line := fmt.Sprintf("%d%%  %s / %s",
			p.current*100/p.total, HumanBytes(p.current), HumanBytes(p.total))
		if p.label != "" {
			line += "  ·  " + p.label
		}
		_, _ = fmt.Fprintf(p.w, "  %s\n", line)
		return
	}
	// Unknown total (stdin streams): a bare byte counter is all a long
	// pipe would ever show — add throughput once it clears HumanRate's
	// noise floor.
	line := HumanBytes(p.current)
	if r := HumanRate(p.current, time.Since(p.start)); r != "" {
		line += "  ·  " + r
	}
	_, _ = fmt.Fprintf(p.w, "  %s\n", line)
}

func (p *plainProgress) setLabel(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.label = name
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
//
// showNames reserves room for the current-file chip (SetLabel). Callers
// pass true only for multi-file transfers — a single file's name is
// already in the pre-transfer block, so the columns go to the bar instead.
func New(totalBytes int64, showNames bool) *Progress {
	// Plain mode for pipes/CI, and for terminals that report a 0×0
	// window (some pty wrappers) — mpb discards every row at height 0.
	width, _, sizeErr := term.GetSize(int(os.Stderr.Fd()))
	if !renderTTY(os.Stderr) || sizeErr != nil || width <= 0 {
		return &Progress{plain: &plainProgress{
			w: os.Stderr, total: totalBytes, lastLine: time.Now(), start: time.Now(),
		}}
	}
	// The fixed width assumes ~55 columns of decorators around the bar; on
	// narrower terminals the line would wrap and the in-place redraw only
	// clears its last visual row, leaving stale fragments behind. The
	// current-file chip widens that budget; terminals too narrow for both
	// keep the bar and drop the chip (wrapping is worse than no name).
	const nameCols = 20 // chip name budget; +5 for its "  ·  " separator
	pad, labelCap := 55, 0
	if showNames && width >= 55+10+nameCols+5 {
		pad += nameCols + 5
		labelCap = nameCols
	}
	bw := min(barWidth, max(10, width-pad))

	p := &Progress{}
	defer setActive(p)

	p.mp = mpb.New(
		mpb.WithOutput(os.Stderr),
		mpb.WithRefreshRate(100*time.Millisecond), // spec: ≥10 Hz
	)

	// The ━/╸/─ trio gives a calm, modern look without the "=====>"
	// telegraph aesthetic the default style carries.
	style := mpb.BarStyle().Lbound(" ").Rbound(" ").Filler("━").Tip("╸").Padding("─")

	// Track elapsed locally — decor.Statistics doesn't carry it. Only the
	// ETA's ≥1 s warm-up gate uses it; the rate itself comes from win, a
	// sliding window, so it tracks current throughput instead of a lifetime
	// mean skewed by a slow handshake or a mid-transfer speed change. The
	// summary line still reports the lifetime figure — that one is correct.
	start := time.Now()
	win := &rateWindow{}
	hasTotal := totalBytes > 0
	// Rate needs no total — a multi-GB stdin stream is exactly where the
	// user wants throughput. HumanRate's own noise floor keeps it hidden
	// until ~1 MB has moved, covering the small-stream case; ETA stays
	// total-gated inside its decorator.
	showRate := !hasTotal || totalBytes >= rateThreshold

	// Percentage on the left. The completed bar is removed (see
	// BarRemoveOnComplete below), so no terminal-state swap is needed.
	prependDecs := []decor.Decorator{
		decor.Name("  "),
		// "%d" renders "66%", matching plain mode — the default
		// "% d" pads a space before the sign ("66 %").
		decor.NewPercentage("%d", decor.WC{W: 4}),
		decor.Name("  "),
	}
	if !hasTotal {
		// Streaming transfers (stdin) — no meaningful percentage.
		prependDecs = []decor.Decorator{
			decor.Name("  "),
			decor.Name("... "),
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
			// of transfer, sub-MB movement — HumanRate's noise floor —
			// or on completion; the summary carries the final figure).
			// This decorator also feeds the window each refresh frame.
			// A stall says "stalled" rather than letting the windowed
			// rate decay through fictional values.
			decor.Any(func(s decor.Statistics) string {
				if s.Completed || s.Aborted || s.Current == 0 {
					return ""
				}
				now := time.Now()
				r := win.observe(now, s.Current)
				if s.Current < rateThreshold {
					return ""
				}
				if win.stalled(now) {
					return "  ·  " + Dim("stalled")
				}
				if r <= 0 {
					return ""
				}
				return "  ·  " + HumanBytes(int64(r)) + "/s"
			}),
			// ETA: needs a known total, non-zero progress, and at least
			// 1 s elapsed so the projection isn't dominated by handshake
			// time. Hidden on completion and during a stall (a projection
			// off a decaying rate only inflates).
			decor.Any(func(s decor.Statistics) string {
				if s.Completed || s.Aborted || s.Total <= 0 || s.Current == 0 {
					return ""
				}
				if time.Since(start) < time.Second || win.stalled(time.Now()) {
					return ""
				}
				rate := win.rate()
				if rate <= 0 {
					return ""
				}
				remainingSecs := float64(s.Total-s.Current) / rate
				if remainingSecs <= 0 {
					return ""
				}
				return "  ·  ETA " + etaLabel(remainingSecs)
			}),
		)
	}
	if labelCap > 0 {
		// Current-file chip, last so its per-file width changes don't
		// jiggle the rate/ETA chips. Kept on aborted bars: the frozen
		// name records which file the transfer stopped in.
		appendDecs = append(appendDecs, decor.Any(func(s decor.Statistics) string {
			name, _ := p.label.Load().(string)
			if name == "" {
				return ""
			}
			return "  ·  " + truncateName(name, labelCap)
		}))
	}

	p.bar = p.mp.New(totalBytes,
		style,
		mpb.BarWidth(bw),
		// Progress is transient: a completed bar erases itself and the
		// summary line that follows is the permanent record. Aborted
		// (partial) bars stay — see Done().
		mpb.BarRemoveOnComplete(),
		mpb.PrependDecorators(prependDecs...),
		mpb.AppendDecorators(appendDecs...),
	)
	return p
}

// etaLabel renders an ETA projection, rounding up to whole seconds: at
// 10 Hz refresh a sub-second projection would flicker millisecond values
// ("ETA 943ms", "ETA 42ms") through the tail of a transfer. Sub-minute
// values are formatted here rather than via HumanDuration, whose ms and
// decimal precision is calibrated for measured elapsed times.
func etaLabel(remainingSecs float64) string {
	secs := int64(math.Ceil(remainingSecs))
	if secs < 1 {
		secs = 1
	}
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	return HumanDuration(time.Duration(secs) * time.Second)
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

// SetLabel sets the current-file chip rendered after the bar's other
// decorators ("  ·  <name>"). Only shown when New was given showNames;
// "" clears it. Callers pass display-safe names (peer-supplied strings
// must be sanitized first) and should call only when the file changes.
func (p *Progress) SetLabel(name string) {
	if p == nil {
		return
	}
	if p.plain != nil {
		p.plain.setLabel(name)
		return
	}
	p.label.Store(name)
}

// truncateName caps s at max runes, cutting in the middle so the tail —
// where the extension lives — stays visible. Mirrors the consent-time
// truncation in cmd/fsend's sanitizer.
func truncateName(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	tail := min(8, max/2)
	return string(r[:max-tail-1]) + "…" + string(r[len(r)-tail:])
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
// Two paths:
//
//   - Bar at 100%: BarRemoveOnComplete erases it; just Wait for mpb
//     to flush so the summary line lands on a clean row.
//   - Bar below 100% (aborted mid-transfer): Abort(false) — keep the
//     partial bar on screen as the record of where it stopped — so mpb
//     releases the bar and Wait returns instead of hanging forever
//     waiting for the bar to reach its declared total.
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
