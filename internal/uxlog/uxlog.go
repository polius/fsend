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
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

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
	pos   atomic.Value // fileSlot: "file i/N" counter (see SetFilePos)
}

// fileSlot is the current-file position behind the chip's "· file 3/12"
// counter. n <= 0 means "no counter".
type fileSlot struct{ i, n int }

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
	fileI    int    // 1-based current file, 0 when unset
	fileN    int    // total files, 0 when unset
	route    string // connection tag chip, "" when unknown
	win      rateWindow
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
		// Clamp to 0..100: a percentage outside that range is never correct
		// output, whatever the caller's accounting did.
		line := fmt.Sprintf("%d%%  %s / %s",
			max(0, min(100, p.current*100/p.total)), HumanBytes(p.current), HumanBytes(p.total))
		// Rate and ETA matter most in CI logs, where nobody watches a bar.
		now := time.Now()
		if r := p.win.observe(now, p.current); p.total >= rateThreshold && r >= 1 && !p.win.stalled(now) {
			line += "  ·  " + HumanBytes(int64(r)) + "/s"
			if left := float64(p.total-p.current) / r; left > 0 {
				line += "  ·  ETA " + etaLabel(left)
			}
		}
		if p.route != "" {
			line += "  ·  " + p.route
		}
		if p.label != "" {
			line += "  ·  " + p.label
		}
		if p.fileN > 0 {
			line += fmt.Sprintf("  ·  file %d/%d", p.fileI, p.fileN)
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
	if p.route != "" {
		line += "  ·  " + p.route
	}
	_, _ = fmt.Fprintf(p.w, "  %s\n", line)
}

func (p *plainProgress) setLabel(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.label = name
}

func (p *plainProgress) setFilePos(i, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.fileI, p.fileN = i, n
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

// barWidth caps the progress bar at a fixed column count — full-width bars
// look "loud" (see CLI UX review notes). It is a cap, not a promise: the bar
// only reaches it when the chips beside it leave the columns.
const barWidth = 40

// barMin floors the bar so even a cramped terminal keeps a track.
const barMin = 10

// The chips' widest forms, booked against the bar up front. mpb hands the bar
// whatever the decorators leave on a given frame, so a chip whose text gained
// a column ("44 MB" vs "43.8 MB") — or that appeared mid-transfer — would
// resize the bar on that frame; a bar that changes width ten times a second
// reads as broken. Booking the widest form costs the bar a few columns on
// terminals where the chips would have stayed hidden for the first second;
// a steady bar is worth them.
const (
	pctFurniture  = 8  // "  100%  ", or the "  ...  " streaming stand-in
	rateFurniture = 15 // "  ·  999.9 MB/s"
	etaFurniture  = 17 // "  ·  ETA 10h02m03s"
	barFurniture  = 2  // the spaces mpb puts around the track
	nameMin       = 12 // runes below which the file chip isn't worth showing
	nameGrow      = 30 // the name grows to this before the bar takes the rest
	barGrow       = 20 // the bar grows to this before the name does
)

// counterFurniture is the widest the counter decorator can render: its two
// leading spaces, the current figure's fixed field, the " / " and the total.
func counterFurniture(total int64) int {
	if total <= 0 {
		return 2 + 8 // streaming: "  " + a maximal HumanBytes figure
	}
	return 2 + counterField(total) + 3 + len(HumanBytes(total))
}

// barLayout is one row's allocation of the terminal's columns.
type barLayout struct {
	bar       int // track width; 0 = no track
	eta, rate bool
	counter   bool
	nameCap   int // file chip's name budget in runes; 0 = no chip
	route     bool
}

// layout splits a row by priority — % > ETA > rate > counter > name > bar
// > route — booking each element's widest form and dropping whole elements
// that don't fit, so mpb never has to cut a number mid-digit. Spare columns
// grow the bar to barGrow, the name to nameGrow, then book the route, then
// the bar to its cap, and the rest goes to the name. Pure and cheap: every
// decorator re-derives it from the live width each frame, so a resize
// re-flows by priority.
func layout(width int, total int64, showNames bool, route string) barLayout {
	var l barLayout
	avail := width - pctFurniture
	take := func(n int) bool {
		if avail >= n {
			avail -= n
			return true
		}
		return false
	}
	l.eta = total >= rateThreshold && take(etaFurniture)
	l.rate = (total <= 0 || total >= rateThreshold) && take(rateFurniture)
	l.counter = take(counterFurniture(total))
	if showNames && take(5+nameMin) {
		l.nameCap = nameMin
	}
	if total > 0 && take(barFurniture+barMin) {
		l.bar = barMin
	}
	grow := func(cur *int, to int) {
		if *cur > 0 && *cur < to {
			g := min(avail, to-*cur)
			*cur += g
			avail -= g
		}
	}
	grow(&l.bar, barGrow)
	grow(&l.nameCap, nameGrow)
	l.route = route != "" && take(len(route)+5)
	grow(&l.bar, barWidth)
	if l.nameCap > 0 {
		l.nameCap += avail
	}
	return l
}

// liveLayout is layout at the terminal's current width, falling back to
// the construction-time width when the size query fails.
func liveLayout(width int, total int64, showNames bool, route string) barLayout {
	return layout(terminalWidth(width), total, showNames, route)
}

// terminalWidth returns stderr's current width, or fallback when stderr
// isn't a measurable terminal. mpb re-samples the size every refresh
// frame; content sized to the width should too, so a mid-transfer resize
// re-flows instead of leaving stale geometry.
func terminalWidth(fallback int) int {
	w, _, err := term.GetSize(int(os.Stderr.Fd()))
	if err != nil || w <= 0 {
		return fallback
	}
	return w
}

// TerminalWidth is terminalWidth for one-shot renders in cmd/fsend
// (e.g. the code box): callers that shape a single block to the
// terminal's width, not a per-frame redraw. Falls back to the given
// width when stderr isn't a measurable terminal.
func TerminalWidth(fallback int) int {
	return terminalWidth(fallback)
}

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
	printlnTo(os.Stderr, line)
}

// printlnTo writes line, first wiping a live spinner frame so the line
// starts at column 0; the spinner redraws below it on its next tick.
func printlnTo(w io.Writer, line string) {
	ttyMu.Lock()
	defer ttyMu.Unlock()
	if activeSpinner() != nil {
		_, _ = fmt.Fprint(w, "\r\x1b[2K")
	}
	_, _ = fmt.Fprintln(w, line)
}

func setActive(p *Progress) {
	activeMu.Lock()
	activeProgress = p
	activeMu.Unlock()
}

// NotifyAfter is how long a run must have taken before Notify fires: a
// transfer the user just watched finish needs no bell.
const NotifyAfter = 10 * time.Second

// Notify flags a successful transfer to a user who may have looked away:
// an OSC 9 desktop notification (iTerm2, WezTerm, Windows Terminal, foot)
// plus one bell. Callers pass the run's wall time; shorter than
// NotifyAfter is a no-op, as is FSEND_NO_NOTIFY. TTY-gated so piped stderr
// stays byte-clean. msg must be caller-controlled literal text — never
// peer-supplied strings, which could inject OSC sequences.
func Notify(msg string, waited time.Duration) {
	if waited < NotifyAfter || NotifyDisabled() || !renderTTY(os.Stderr) {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "\x1b]9;fsend: %s\x07\a", msg)
}

// NotifyDisabled reports the FSEND_NO_NOTIFY opt-out.
func NotifyDisabled() bool {
	v := os.Getenv("FSEND_NO_NOTIFY")
	return v != "" && v != "0" && v != "false"
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
//
// route is the connection tag ("local network"/"direct"/"relay"), always
// known before the first byte — data only flows on an established path.
// Rendered as a dim chip so a long transfer stays honest about which
// path it rides.
func New(totalBytes int64, showNames bool, route string) *Progress {
	// Plain mode for pipes/CI, and for terminals that report a 0×0
	// window (some pty wrappers) — mpb discards every row at height 0.
	width := terminalWidth(0)
	if !renderTTY(os.Stderr) || width <= 0 {
		return &Progress{plain: &plainProgress{
			w: os.Stderr, total: totalBytes, lastLine: time.Now(), start: time.Now(), route: route,
		}}
	}
	// Only the bar's cap is fixed here (so it can't jiggle as chips come
	// and go); every chip re-derives from the live width each refresh
	// frame, so a mid-transfer resize re-flows by priority. A bar of 1 is
	// "no track": the filler draws nothing narrower than its bounds.
	bw := max(1, layout(width, totalBytes, showNames, route).bar)
	lay := func() barLayout { return liveLayout(width, totalBytes, showNames, route) }

	p := &Progress{}
	defer setActive(p)

	p.mp = mpb.New(
		mpb.WithOutput(os.Stderr),
		mpb.WithRefreshRate(100*time.Millisecond), // spec: ≥10 Hz
	)

	hasTotal := totalBytes > 0
	// The ━/╸/─ trio gives a calm, modern look without the "=====>"
	// telegraph aesthetic the default style carries. Unknown totals get
	// no track at all: it could never fill, and a full-width ─ line at
	// 0% reads as "stalled" — the counter/rate decorators carry the line.
	var style mpb.BarFillerBuilder = mpb.BarStyle().Lbound(" ").Rbound(" ").Filler("━").Tip("╸").Padding("─")
	if !hasTotal {
		style = mpb.NopStyle()
	}

	// Track elapsed locally — decor.Statistics doesn't carry it. Only the
	// ETA's ≥1 s warm-up gate uses it; the rate itself comes from win, a
	// sliding window, so it tracks current throughput instead of a lifetime
	// mean skewed by a slow handshake or a mid-transfer speed change. The
	// summary line still reports the lifetime figure — that one is correct.
	start := time.Now()
	win := &rateWindow{}
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
		if !lay().counter {
			return ""
		}
		return "  " + counterLine(s.Current, s.Total)
	})

	appendDecs := []decor.Decorator{counters}
	// The plain (un-dimmed) stalled marker; decor.Meta colors it below while
	// mpb measures the bar width off this ANSI-free form.
	const stalledChip = "  ·  stalled"
	if showRate {
		appendDecs = append(appendDecs,
			// Rate: hidden when the figure would be misleading (start
			// of transfer, sub-MB movement — HumanRate's noise floor —
			// or on completion; the summary carries the final figure).
			// This decorator also feeds the window each refresh frame.
			// A stall says "stalled" rather than letting the windowed
			// rate decay through fictional values.
			decor.Meta(
				decor.Any(func(s decor.Statistics) string {
					if s.Completed || s.Aborted || s.Current == 0 {
						return ""
					}
					now := time.Now()
					r := win.observe(now, s.Current) // feeds the ETA even when hidden
					if s.Current < rateThreshold || !lay().rate {
						return ""
					}
					if win.stalled(now) {
						return stalledChip
					}
					// Below 1 B/s the figure rounds to "0 B/s"; show nothing
					// rather than a rate that reads as stalled.
					if r < 1 {
						return ""
					}
					return "  ·  " + HumanBytes(int64(r)) + "/s"
				}),
				// Colour only the stalled marker — the warning token, the
				// caution family, since a stall is a soft warning. Measuring
				// width off the plain string above keeps mpb from counting
				// the ANSI escapes — a colorized decor.Any costs ~6 columns
				// of bar during a stall.
				func(str string) string {
					if str == stalledChip {
						return fg(tokenWarning) + str + colorReset
					}
					return str
				},
			),
			// ETA: needs a known total, non-zero progress, and at least
			// 1 s elapsed so the projection isn't dominated by handshake
			// time. Hidden on completion and during a stall (a projection
			// off a decaying rate only inflates).
			decor.Any(func(s decor.Statistics) string {
				if s.Completed || s.Aborted || s.Total <= 0 || s.Current == 0 || !lay().eta {
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
	if route != "" {
		// Route chip, plain default foreground — the bar's other
		// decorators provide the emphasis around it. Dropped first when
		// the terminal is too narrow to hold it beside the bar and the
		// file chip: the route was already stated on the phase line
		// above, so it is the cheapest thing to lose.
		appendDecs = append(appendDecs, decor.Any(func(decor.Statistics) string {
			if !lay().route {
				return ""
			}
			return "  ·  " + route
		}))
	}
	if showNames {
		// Current-file chip, last so its per-file width changes don't
		// jiggle the rate/ETA chips. Two decorators, two tones: the name
		// wears the file-reference green, while the "file i/N" counter
		// stays in the default foreground so it reads as progress instead
		// of another filename. The budget re-derives from the live width
		// every frame, so a mid-transfer resize re-truncates (or drops)
		// the chip; terminalWidth falls back to the width seen at
		// construction if the size query starts failing.
		appendDecs = append(appendDecs,
			decor.Meta(decor.Any(func(decor.Statistics) string {
				name, _ := p.chipParts(lay().nameCap)
				return name
			}), func(str string) string { return Path(str) }),
			decor.Any(func(decor.Statistics) string {
				_, tail := p.chipParts(lay().nameCap)
				return tail
			}),
		)
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

// counterField is the width of the current figure's right-aligned field.
// HumanBytes trims a ".0" tail ("44 MB" vs "43.8 MB"), and a figure just
// under the total can carry a digit more than it ("1.0 GB" arrives as
// "1000 MB"), so the field is the total's own width plus one — or the eight
// columns the widest rendering ("999.9 KB") needs.
func counterField(total int64) int {
	if total <= 0 {
		return 8
	}
	return max(8, len(HumanBytes(total))+1)
}

// counterLine renders the "transferred / total" counter, the current figure
// right-aligned in counterField's fixed width so the counter occupies the same
// columns on every frame.
func counterLine(current, total int64) string {
	if total <= 0 {
		return HumanBytes(current)
	}
	return fmt.Sprintf("%*s / %s", counterField(total), HumanBytes(current), HumanBytes(total))
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

// SetFilePos sets the "file i/N" counter the chip appends after the name.
// i is 1-based. This is the honest progress signal when a folder holds
// many small files and the byte bar barely moves; n <= 0 clears it. Safe
// on a nil receiver.
func (p *Progress) SetFilePos(i, n int) {
	if p == nil {
		return
	}
	if p.plain != nil {
		p.plain.setFilePos(i, n)
		return
	}
	p.pos.Store(fileSlot{i, n})
}

// filePosition returns the stored file position, or (0, 0) when unset.
func (p *Progress) filePosition() (int, int) {
	if v, ok := p.pos.Load().(fileSlot); ok {
		return v.i, v.n
	}
	return 0, 0
}

// chipParts splits the current-file chip into its two tones: the name and
// the "· file i/N" counter. chipCap is the chip's rune budget from
// layoutBudgets. Either half may come back empty — the name when there is
// nothing to show, the counter when the name would be squeezed below a
// readable stub.
func (p *Progress) chipParts(chipCap int) (name, tail string) {
	label, _ := p.label.Load().(string)
	if label == "" || chipCap == 0 {
		return "", ""
	}
	if i, n := p.filePosition(); n > 0 {
		if t := fmt.Sprintf("  ·  file %d/%d", i, n); chipCap-len(t) >= 6 {
			tail = t
			chipCap -= len(t)
		}
	}
	return "  ·  " + truncateName(label, chipCap), tail
}

// truncateName caps s at max runes with a middle cut: the whole basename
// stays when it fits ("assets/…/video1.bin"), else the basename itself is
// cut in the middle, keeping its start and extension ("vid…o1.bin") — a
// left cut ("…g.bin") hides the part that identifies the file. Runes, not
// display cells: a name heavy in wide glyphs can overflow the chip's
// budget, and mpb then trims the line's end rather than wrapping it.
func truncateName(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	switch {
	case max <= 0:
		return ""
	case max == 1:
		return "…"
	}
	keep := max - 1
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		base := s[i+1:]
		switch n := utf8.RuneCountInString(base); {
		case n+1 <= keep-2: // "head…/basename"
			return string(r[:keep-n-1]) + "…" + s[i:]
		case n <= keep: // "…basename"
			return "…" + base
		}
		r = []rune(base) // cut inside the basename itself
	}
	tail := keep * 2 / 3
	return string(r[:keep-tail]) + "…" + string(r[len(r)-tail:])
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
