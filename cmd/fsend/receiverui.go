package main

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/polius/fsend/internal/atomicfile"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/conv"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/retry"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
	"github.com/polius/fsend/internal/wire"
)

// receiverUI bundles the receive-side engine callbacks (accept prompt,
// progress bar, overwrite confirm) with shared state. One instance serves a
// whole receive, including retries.
type receiverUI struct {
	ctx      context.Context
	f        *flags
	outDir   string
	sink     bool
	pathInfo connpath.Info
	// spin animates the pre-prompt phase (connect/classify); set before the
	// transfer starts, stopped by the first prompt or close(). Stop is
	// idempotent and nil-safe.
	spin *uxlog.Spinner

	mu           sync.Mutex
	hello        *wire.SenderHello
	files        []string          // paths of what landed this run
	names        map[uint32]string // index → sanitized name; multi-file only
	ordinal      map[uint32]int    // index → 1-based position; multi-file only
	stripPrefix  string            // shared wrapper dir trimmed from display names
	curFile      uint32            // last index labelled on the bar
	prev         map[uint32]uint64
	total        int64 // bytes received this run (excludes resumed prefixes)
	skipped      int64 // resumed prefixes already on disk
	skippedSame  int   // files skipped because they were identical
	kept         int   // differing/conflicting entries left untouched
	keptByChoice bool  // kept because the user chose "skip" at the prompt
	bar          *uxlog.Progress
	firstByte    time.Time
	bytesHint    int64
	diffBytes    int64 // bytes that move only if overwrite is approved
	manifestErr  error // a --manifest write failure, surfaced after the transfer

	// choice is the answer to "what about files that already exist",
	// asked together with the accept prompt and handed to the engine.
	announced bool // the peer and accepted events went out (once per receive)
	jp        jsonProgress

	choice   transfer.ConflictPolicy
	notSaved []string // entries not saved for other reasons (unreadable, unplaceable)
	changed  []string // files the sender saw change while reading them

	closeOnce sync.Once
}

func newReceiverUI(ctx context.Context, f *flags, outDir string, sink bool, pathInfo connpath.Info) *receiverUI {
	// curFile starts at a sentinel no wire index uses, so file 0 still
	// triggers the first bar label.
	return &receiverUI{ctx: ctx, f: f, outDir: outDir, sink: sink, pathInfo: pathInfo,
		prev: make(map[uint32]uint64), curFile: ^uint32(0)}
}

// resolveOutDir resolves --out: "-" selects sink (stdout); "" defaults to CWD.
func resolveOutDir(f *flags) (dir string, sink bool, err error) {
	if f.outDir == "-" {
		return "", true, nil
	}
	dir = f.outDir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", false, err
		}
	}
	if abs, absErr := filepath.Abs(dir); absErr == nil {
		dir = abs
	}
	if f.outDir != "" {
		st, statErr := os.Stat(dir)
		switch {
		case statErr == nil && !st.IsDir():
			return "", false, fmt.Errorf("%w: --out is not a directory: %s", fserrors.ErrUsage, dir)
		case os.IsNotExist(statErr):
			// Create it (and parents), like `mkdir -p` — failing after the
			// one-shot code is consumed just to say "make this first" is poor UX.
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return "", false, fmt.Errorf("%w: could not create --out directory %s: %v", fserrors.ErrUsage, dir, mkErr)
			}
		case statErr != nil:
			return "", false, fmt.Errorf("%w: cannot access --out directory %s: %v", fserrors.ErrUsage, dir, statErr)
		}
	}
	return dir, false, nil
}

func (ui *receiverUI) recvOptions(hostname string) transfer.RecvOptions {
	o := transfer.RecvOptions{
		Hostname:       hostname,
		OS:             runtime.GOOS,
		ClientVersion:  version.Version,
		TargetDir:      ui.outDir,
		Checksum:       ui.f.checksum,
		Accept:         ui.accept,
		Password:       ui.f.passArg,
		ProgressFn:     ui.progress,
		OnResume:       ui.onResume,
		OnSkip:         ui.onSkip,
		OnFileDone:     ui.onFileDone,
		OnConflictKept: ui.onConflictKept,

		ResolveConflicts: ui.resolveConflicts,
		OnEntrySkipped:   ui.onEntrySkipped,
		OnSourceChanged:  ui.onSourceChanged,
		OnHashProgress: func(done, total uint64) {
			if total > 0 {
				ui.spin.SetMessage(fmt.Sprintf("Checking existing files  %d%%", done*100/total))
			}
		},
	}
	// The password prompt fires before classification; the spinner must not
	// animate under the hidden-input line.
	if pp := receiverPasswordPrompt(ui.ctx, ui.f); pp != nil {
		o.PromptPass = func(attempt int) (string, error) {
			ui.spin.Stop()
			return pp(attempt)
		}
	}
	if ui.sink {
		o.Sink = os.Stdout
	}
	if ui.f.manifest != "" {
		o.OnManifest = ui.onManifest
	}
	return o
}

// flagPolicy is the conflict answer given on the command line, if any.
// Without one, --yes/--quiet keep the local copies (no prompt to ask).
func (ui *receiverUI) flagPolicy() (transfer.ConflictPolicy, bool) {
	switch {
	case ui.f.overwrite:
		return transfer.ConflictReplace, true
	case ui.f.rename:
		return transfer.ConflictRename, true
	}
	return transfer.ConflictKeep, false
}

// resolveConflicts hands the engine the answer the accept prompt collected
// (or the flag's), and sizes the bar for the files that will now move.
func (ui *receiverUI) resolveConflicts([]transfer.Conflict) transfer.ConflictPolicy {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.choice != transfer.ConflictKeep {
		ui.bytesHint += ui.diffBytes
	}
	return ui.choice
}

// accept records the HELLO, then runs the accept prompt with the breakdown.
func (ui *receiverUI) accept(h wire.SenderHello, summary transfer.ClassifySummary) bool {
	// Classification is done — the prompt block owns the terminal from here.
	ui.spin.Stop()
	ui.mu.Lock()
	cp := h
	ui.hello = &cp
	ui.bytesHint = conv.Int64(summary.BytesToRecv)
	ui.diffBytes = conv.Int64(summary.DifferingBytes)
	ui.choice, _ = ui.flagPolicy()
	// Current-file chip data: multi-file only. Names are peer-supplied, so
	// sanitize here — the only place they enter the UI state.
	if h.Mode == wire.ModeFiles && len(summary.Files) > 1 {
		// Current-file chip data: multi-file only. Names are peer-supplied,
		// so sanitize here — the only place they enter the UI state. Trim
		// the shared wrapping directory the incoming headline already
		// names (same trim as the sender's chip and the preview list), and
		// keep a 1-based ordinal for the "file i/N" counter.
		items := make([]previewItem, 0, len(summary.Files))
		for _, e := range summary.Files {
			items = append(items, previewItem{name: sanitizeForDisplay(e.RelativePath, 128)})
		}
		ui.stripPrefix = stripCommonDir(items)
		ui.names = make(map[uint32]string, len(summary.Files))
		ui.ordinal = make(map[uint32]int, len(summary.Files))
		for i, e := range summary.Files {
			ui.names[e.Index] = items[i].name
			ui.ordinal[e.Index] = i + 1
		}
	}
	first := !ui.announced
	ui.announced = true
	ui.mu.Unlock()
	if first {
		jsonEmitPeer(sanitizeRemote(h.Hostname), jsonRoute(ui.pathInfo.Kind))
	}
	ok := ui.promptAccept(h, summary)
	if ok && first {
		jsonEmitAccepted()
	}
	return ok
}

// promptAccept renders the incoming-transfer block and asks whether to save.
func (ui *receiverUI) promptAccept(h wire.SenderHello, summary transfer.ClassifySummary) bool {
	f := ui.f
	if f.quiet {
		return f.yes
	}
	route := ""
	if ui.pathInfo.Kind != connpath.KindUnknown {
		route = ui.pathInfo.Chip()
	}
	fmt.Fprintln(os.Stderr)
	renderArtifact(os.Stderr, h, summary, ui.sink, sanitizeRemote(h.Hostname), route)
	fmt.Fprintln(os.Stderr)

	// Nothing would be written — every file is already here, identical — so
	// there is nothing to consent to. Accepting lets both sides record the
	// "already up to date" outcome.
	if h.Mode == wire.ModeFiles && summary.NewItems == 0 && summary.Differing == 0 &&
		summary.BytesToRecv == 0 && len(summary.Conflicts) == 0 && summary.Refused == 0 && summary.Identical > 0 && !ui.sink {
		return true
	}
	// Text is sanitized and printed, never saved: asking before showing a
	// short note only adds a step. The hello carries no size, so a long
	// payload is asked about once it has arrived (finishReceive).
	if h.Mode == wire.ModeStream && h.IsText && !ui.sink && stdoutIsTTY() {
		return true
	}
	_, flagged := ui.flagPolicy()
	if f.yes {
		fmt.Fprintln(os.Stderr, uxlog.Detail("Accepting (--yes)"))
		// --yes answers the accept prompt, not the existing-files question —
		// differing files are kept and the exit will be E013. Scripts reading
		// --yes as "yes to everything" deserve to hear that now.
		if summary.Differing > 0 && !flagged {
			fmt.Fprintf(os.Stderr, "%s Keeping %s — pass --overwrite to replace, or --rename to keep both\n",
				uxlog.Warn(), uxlog.CountNoun(summary.Differing, "differing file"))
		}
		return true
	}
	// Every line that wants an answer leads with the ? gutter, so a glance
	// down the left edge finds it; the destination is the line's subject.
	var question string
	switch {
	case ui.sink:
		question = "Write to stdout?"
	case h.Mode == wire.ModeStream && h.IsText:
		question = "Show the text?"
	default:
		question = "Save to " + uxlog.Subject(saveTargetLabel(ui.outDir))
	}
	// Files that already exist: one question covers both the accept and what
	// to do with them, instead of a second prompt after "yes".
	if len(summary.Conflicts) > 0 && !flagged && !ui.sink {
		return ui.askWithConflicts(question, summary.Conflicts)
	}
	for {
		fmt.Fprintf(os.Stderr, "%s %s  [Y/n] ", uxlog.Ask(), question)
		line, eof, ok := readLineCtx(ui.ctx)
		if !ok {
			return false
		}
		if eof {
			fmt.Fprintf(os.Stderr, "\n%s No input to answer the prompt — declining. Pass --yes to accept automatically.\n", uxlog.Info())
			return false
		}
		switch line {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(os.Stderr, "  Please answer y or n.")
	}
}

// askWithConflicts is the accept prompt when some incoming files already
// exist and differ. It lists them and asks once: keep both (the default —
// nothing is lost), replace, skip them, or decline the whole transfer.
func (ui *receiverUI) askWithConflicts(question string, conflicts []transfer.Conflict) bool {
	const preview = 5
	names := ui.conflictNames(conflicts)
	fmt.Fprintf(os.Stderr, "%s %s here already %s and %s\n", uxlog.Ask(),
		uxlog.CountNoun(len(conflicts), "file"), pick(len(conflicts) == 1, "exists", "exist"), differVerb(len(conflicts)))
	shown := min(len(conflicts), preview)
	if len(conflicts) == preview+1 {
		shown = len(conflicts) // never hide exactly one row
	}
	rows := conflictRows(conflicts, names)
	for _, r := range rows[:shown] {
		fmt.Fprintf(os.Stderr, "    %s\n", r)
	}
	if len(conflicts) > shown {
		fmt.Fprintf(os.Stderr, "    %s\n", uxlog.Muted(fmt.Sprintf("… and %d more — l lists all %d", len(conflicts)-shown, len(conflicts))))
	}
	if allLocalNewer(conflicts) {
		mine := pick(len(conflicts) == 1, "Yours is newer", "All of yours are newer")
		fmt.Fprintf(os.Stderr, "%s %s — replace would lose your changes\n", uxlog.Warn(), mine)
	}
	options := "[K]eep both · [r]eplace " + uxlog.Alert("(deletes yours)") + " · [s]kip them · [n]o"
	for {
		fmt.Fprintf(os.Stderr, "%s %s  %s ", uxlog.Ask(), question, options)
		line, eof, ok := readLineCtx(ui.ctx)
		if !ok {
			return false
		}
		if eof {
			fmt.Fprintf(os.Stderr, "\n%s No input to answer the prompt — declining. Pass --yes to accept automatically.\n", uxlog.Info())
			return false
		}
		choice := transfer.ConflictKeep
		switch line {
		case "", "k", "keep", "b", "both", "y", "yes":
			choice = transfer.ConflictRename
		case "r", "replace":
			choice = transfer.ConflictReplace
		case "s", "skip":
		case "n", "no":
			return false
		case "l", "list":
			for _, r := range rows {
				fmt.Fprintf(os.Stderr, "    %s\n", r)
			}
			continue
		default:
			fmt.Fprintln(os.Stderr, "  Please answer k (keep both), r (replace), s (skip) or n (cancel).")
			continue
		}
		ui.mu.Lock()
		ui.choice = choice
		// Skipping is a decision, not a failure: the summary says so with ℹ
		// and the exit stays 0.
		ui.keptByChoice = choice == transfer.ConflictKeep
		ui.mu.Unlock()
		return true
	}
}

func pick(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

// conflictNames returns the display names for the conflict rows, trimmed of
// the shared wrapping directory the incoming headline already names (the same
// trim the bar chip and the preview list do), so a conflict row doesn't repeat
// "myproject/" and burn the columns on the part that never changes.
func (ui *receiverUI) conflictNames(conflicts []transfer.Conflict) []string {
	names := make([]string, len(conflicts))
	for i, c := range conflicts {
		names[i] = sanitizeForDisplay(c.RelativePath, 128)
	}
	if p := ui.stripPrefix; p != "" {
		for i := range names {
			names[i] = strings.TrimPrefix(names[i], p)
		}
	}
	return names
}

// compactAgo renders an age in the shortest honest unit — "30 s", "4 min",
// "3 h", "2 d" — for the conflict rows.
func compactAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	default:
		return fmt.Sprintf("%d d", int(d.Hours()/24))
	}
}

// sameAgeSlack absorbs filesystem timestamp precision (FAT keeps 2 s, the
// listing whole seconds) when deciding which copy is newer.
const sameAgeSlack = 2 * time.Second

// newer reports which copy of a differing file is newer: > 0 yours, < 0
// theirs, 0 the same age or unknown.
func newer(c transfer.Conflict) time.Duration {
	if c.LocalModTime.IsZero() || c.IncomingModTime.IsZero() {
		return 0
	}
	d := c.LocalModTime.Sub(c.IncomingModTime)
	if d.Abs() < sameAgeSlack {
		return 0
	}
	return d
}

// conflictLabel renders one conflict's facts, after its name: a type clash
// names its kind; a "differs" row the size change, old → new, and which
// copy is newer — for two edits of one document the deciding fact, since
// "replace" deletes yours. When the rounded sizes collide and would read as
// a no-op, it also says what actually differs: the byte delta. No colour:
// the name is the subject and the question above the rows carries the
// attention.
func conflictLabel(c transfer.Conflict) string {
	if c.Kind != "differs" {
		return c.Kind
	}
	row := uxlog.HumanBytes(c.LocalSize) + " → " + uxlog.HumanBytes(conv.Int64(c.IncomingSize))
	if uxlog.HumanBytes(c.LocalSize) == uxlog.HumanBytes(conv.Int64(c.IncomingSize)) {
		if d := conv.Int64(c.IncomingSize) - c.LocalSize; d != 0 {
			row += fmt.Sprintf(" (%+d B)", d)
		}
	}
	switch d := newer(c); {
	case d > 0:
		row += "   yours is newer by " + compactAgo(d)
	case d < 0:
		row += "   theirs is newer by " + compactAgo(-d)
	case !c.IncomingModTime.IsZero() && !c.LocalModTime.IsZero():
		row += "   same age"
	case !c.LocalModTime.IsZero():
		row += "   yours modified " + compactAgo(time.Since(c.LocalModTime)) + " ago"
	}
	return row
}

// allLocalNewer reports whether every differing local copy is newer than
// the incoming one — the case where "replace" loses work.
func allLocalNewer(conflicts []transfer.Conflict) bool {
	n := 0
	for _, c := range conflicts {
		if c.Kind != "differs" {
			continue
		}
		if newer(c) <= 0 {
			return false
		}
		n++
	}
	return n > 0
}

// conflictRows aligns the conflict list: names padded to one column (up to
// a cap, so one long path doesn't push the rest off screen), facts faint.
func conflictRows(conflicts []transfer.Conflict, names []string) []string {
	w := 0
	for _, n := range names {
		w = max(w, min(nameColumnCap, utf8.RuneCountInString(n)))
	}
	rows := make([]string, len(conflicts))
	for i, c := range conflicts {
		pad := strings.Repeat(" ", max(0, w-utf8.RuneCountInString(names[i])))
		rows[i] = names[i] + pad + "   " + uxlog.Muted(conflictLabel(c))
	}
	return rows
}

func (ui *receiverUI) onResume(fileIndex uint32, offset, total uint64) {
	ui.mu.Lock()
	if offset > ui.prev[fileIndex] {
		d := conv.Int64(offset - ui.prev[fileIndex])
		ui.prev[fileIndex] = offset
		ui.skipped += d
		// bytesHint counts only bytes still to receive (classify deducts
		// the partial's aligned offset), but the bar's numerator counts the
		// resumed prefix too — grow the total by the same delta or the bar
		// reads past 100%. Both sides then show absolute-over-full-size,
		// matching the sender.
		ui.bytesHint += d
		if ui.bar == nil {
			ui.ensureBar()
		} else {
			ui.bar.SetTotal(ui.bytesHint, false)
		}
		ui.bar.Add(d)
	}
	quiet := ui.f.quiet
	ui.mu.Unlock()
	if !quiet {
		uxlog.Note(resumeNotice(offset, total))
	}
}

// ensureBar creates the progress bar for a transfer that earns one (see
// wantsBar). Callers hold ui.mu.
func (ui *receiverUI) ensureBar() {
	if ui.bar != nil || ui.f.quiet {
		return
	}
	stream := ui.hello != nil && ui.hello.Mode == wire.ModeStream && !ui.hello.IsText
	if wantsBar(ui.bytesHint, stream) {
		ui.bar = uxlog.New(ui.bytesHint, ui.names != nil, ui.pathInfo.BarTag())
	}
}

func (ui *receiverUI) onSkip(uint32) {
	ui.mu.Lock()
	ui.skippedSame++
	ui.mu.Unlock()
}

func (ui *receiverUI) onConflictKept(string) {
	ui.mu.Lock()
	ui.kept++
	ui.mu.Unlock()
}

// onEntrySkipped records an entry that wasn't saved for a reason other than
// the user's choice; finishReceive lists them and exits E050.
func (ui *receiverUI) onEntrySkipped(rel, reason string) {
	ui.mu.Lock()
	ui.notSaved = append(ui.notSaved, sanitizeForDisplay(rel, 128)+" — "+reason)
	ui.mu.Unlock()
}

func (ui *receiverUI) onSourceChanged(rel string) {
	ui.mu.Lock()
	ui.changed = append(ui.changed, sanitizeForDisplay(rel, 128))
	ui.mu.Unlock()
}

// onManifest writes the post-transfer record (path,size,status) as CSV to the
// --manifest path. Fired only after a successful receive, so the file isn't
// left describing a transfer that failed.
func (ui *receiverUI) onManifest(entries []transfer.ManifestEntry) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"path", "size", "status", "sha256"})
	for _, e := range entries {
		_ = cw.Write([]string{csvSafe(e.RelativePath), strconv.FormatUint(e.Size, 10), csvSafe(e.Status), e.SHA256})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		ui.manifestErr = fmt.Errorf("%w: %s: %v", fserrors.ErrManifestWriteFailed, ui.f.manifest, err)
		return
	}
	// Atomic write (tmp + fsync + rename): a crash or a concurrent reader
	// never sees a truncated manifest describing a completed transfer.
	if err := atomicfile.Write(ui.f.manifest, buf.Bytes(), 0o644); err != nil {
		ui.manifestErr = fmt.Errorf("%w: %s: %v", fserrors.ErrManifestWriteFailed, ui.f.manifest, err)
	}
}

// csvSafe defuses spreadsheet formula injection. A cell whose first byte is
// =, +, -, @ (or a tab/CR that a parser skips before one) is evaluated as a
// formula when the CSV is opened in Excel/Sheets, which can exfiltrate data
// or run a DDE payload. A leading apostrophe is the standard defang: the
// spreadsheet treats the remainder as literal text. Paths and statuses are
// the attacker-influenced fields; size and sha256 are numeric/hex.
func csvSafe(s string) string {
	switch {
	case s == "":
		return s
	case strings.ContainsRune("=+-@\t\r", rune(s[0])):
		return "'" + s
	}
	return s
}

func (ui *receiverUI) progress(fileIndex uint32, bytesWritten uint64) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.firstByte.IsZero() {
		ui.firstByte = time.Now()
		ui.ensureBar()
	}
	if name, ok := ui.names[fileIndex]; ok && fileIndex != ui.curFile {
		ui.curFile = fileIndex
		ui.bar.SetLabel(name)
		ui.bar.SetFilePos(ui.ordinal[fileIndex], len(ui.names))
	}
	// Clamp: on a retry bytesWritten can dip below the prior attempt's
	// high-water mark (prev persists across attempts), and unsigned
	// subtraction would wrap into a huge negative bar/summary.
	d := uint64(0)
	if bytesWritten > ui.prev[fileIndex] {
		d = bytesWritten - ui.prev[fileIndex]
	}
	ui.prev[fileIndex] = bytesWritten
	ui.total += int64(d)
	ui.bar.Add(int64(d))
	ui.jp.emit(ui.total+ui.skipped, ui.bytesHint, ui.names[fileIndex])
}

// onFileDone records each saved file's path for the summary headline and
// the JSON done event. (Per-file SHA-256 hashes are not surfaced in the
// terminal — they flooded it after folder receives; --manifest's CSV is
// the verification surface.)
func (ui *receiverUI) onFileDone(path, _ string) {
	ui.mu.Lock()
	ui.files = append(ui.files, path)
	ui.mu.Unlock()
}

// resetAttemptCounts drops the file tallies before a retry re-runs the
// transfer. A fresh attempt reclassifies against disk and re-fires these
// callbacks, so keeping the prior attempt's counts would double them (a file
// saved then reclassified as identical would show as both). Byte counters
// (prev/total/skipped) are keyed per file and self-correct, so they persist.
func (ui *receiverUI) resetAttemptCounts() {
	ui.mu.Lock()
	ui.files = ui.files[:0]
	ui.skippedSame = 0
	ui.kept = 0
	ui.notSaved = nil
	ui.changed = nil
	ui.mu.Unlock()
}

func (ui *receiverUI) bytes() (total, moved int64) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	return ui.total + ui.skipped, ui.total
}

// printCancelKeptHint records, for a Ctrl-C'd receiver whose partial data
// survives, where it stopped — renderError then prints the block under
// its headline:
//
//	! Cancelled at 62%
//	  Partial data kept in ./.fsend
//	  Resume while the sender waits: fsend qxq-aaud-zar
//
// E026 itself is context-free ("Cancelled."); only the UI knows whether
// bytes had already landed. The bar closes first: its final frame is the
// record of where the transfer stopped, and the block belongs under it.
func printCancelKeptHint(f *flags, ui *receiverUI) {
	if f.quiet {
		return
	}
	if _, moved := ui.bytes(); moved > 0 {
		ui.close()
		total, _ := ui.bytes()
		ui.mu.Lock()
		hint := ui.bytesHint
		ui.mu.Unlock()
		cancelKept = &cancelState{
			pct:     uxlog.Percent(total, hint),
			partial: displayPath(transfer.PartialDir(ui.outDir)),
		}
	}
}

// cancelState is a cancelled receive's partial-data record, for the
// cancellation block renderError prints. nil when nothing was kept.
type cancelState struct {
	pct     int
	partial string
}

var cancelKept *cancelState

// resumeCommand is the exact command that resumes a receive: the same
// code, into the same directory.
func resumeCommand(f *flags, code string) string {
	cmd := "fsend " + code
	if f.outDir != "" && f.outDir != "-" {
		cmd += " --out " + f.outDir
	}
	return cmd
}

func (ui *receiverUI) close() {
	ui.closeOnce.Do(func() {
		ui.spin.Stop()
		ui.mu.Lock()
		bar := ui.bar
		ui.mu.Unlock()
		if bar != nil {
			bar.Done()
		}
	})
}

// retryNotice wraps retryNoticeFor so the pre-prompt spinner is stopped
// before a notice prints — otherwise the spinner's redraw garbles the line.
func (ui *receiverUI) retryNotice() func(int, time.Duration, error) {
	notice := retryNoticeFor(ui.f)
	return func(attempt int, wait time.Duration, lastErr error) {
		ui.spin.Stop()
		jsonEmitRetry(attempt, retry.DefaultAttempts, wait)
		if notice != nil {
			notice(attempt, wait, lastErr)
		}
	}
}

// finishReceive runs the post-transfer epilogue. When differing files were
// kept (no consent) it returns E013 so scripts get a non-zero exit, after
// showing the summary; a --manifest write failure surfaces the same way.
func finishReceive(f *flags, ui *receiverUI, elapsed time.Duration) error {
	// Unknown-total bars (stream/text: bytesHint 0) never reach Completed on
	// their own, so Done() would keep them — the keep-the-bar policy is meant
	// for aborts, and this is the success path. Latch the total so the bar
	// erases itself, mirroring the sender's onStreamingEOF.
	ui.mu.Lock()
	if ui.bar != nil && ui.bytesHint == 0 {
		ui.bar.SetTotal(ui.total+ui.skipped, true)
	}
	ui.mu.Unlock()
	ui.close()

	ui.mu.Lock()
	h := ui.hello
	files := append([]string(nil), ui.files...)
	firstByte := ui.firstByte
	kept := ui.kept
	keptByChoice := ui.keptByChoice
	skippedSame := ui.skippedSame
	skipped := append([]string(nil), ui.notSaved...)
	changed := append([]string(nil), ui.changed...)
	ui.mu.Unlock()

	if !firstByte.IsZero() {
		elapsed = time.Since(firstByte)
	}

	total, moved := ui.bytes()
	text := ""
	isText := h != nil && h.Mode == wire.ModeStream && h.IsText && !ui.sink
	if isText {
		payload, err := readTextPayload(files)
		if err != nil {
			return err
		}
		text = payload
		// Under --json the payload rides in the done event instead of
		// raw stdout, which must stay pure NDJSON.
		if !jsonEnabled() && ui.showText(text) {
			if err := printTextPayload(text); err != nil {
				return err
			}
		}
		printRecvSummary(f, "Received text", total, moved, kept, 0, keptByChoice, elapsed, ui.pathInfo)
	} else {
		headline := ui.headline(h, files)
		// With kept-back or skipped files the peer-supplied display name ("2
		// files") would overcount what landed — say how many were written.
		if (kept > 0 || len(skipped) > 0) && !ui.sink {
			headline = fmt.Sprintf("Saved %d of %s to %s",
				len(files), uxlog.CountNoun(len(files)+skippedSame+kept+len(skipped), "file"), saveTargetLabel(ui.outDir))
		}
		printRecvSummary(f, headline, total, moved, kept, skippedSame, keptByChoice, elapsed, ui.pathInfo)
		printSkippedList(f, "", skipped)
		if !f.quiet {
			for _, c := range changed {
				fmt.Fprintf(os.Stderr, "%s %s changed on the sender while it was being sent — the copy may be inconsistent\n", uxlog.Warn(), c)
			}
		}
	}
	// manifestErr is set by onManifest, which runs on this goroutine before we
	// return, so no lock is needed. The transfer succeeded; the failure is only
	// that --manifest couldn't be written.
	var retErr error
	switch {
	case ui.manifestErr != nil:
		retErr = ui.manifestErr
	case len(skipped) > 0:
		retErr = fmt.Errorf("%w: %d not saved", fserrors.ErrFilesSkipped, len(skipped))
	case kept > 0 && keptByChoice:
		// The user chose to skip them: a decision, not a failure.
	case kept > 0:
		retErr = fserrors.ErrTargetExists
	}
	ev := jsonDoneEvent{Ok: retErr == nil, Role: "receiver",
		BytesTotal: ptr64(total), BytesMoved: ptr64(moved),
		DurationMS: msPtr(elapsed), Route: jsonRoute(ui.pathInfo.Kind), Text: text}
	if retErr != nil {
		entry, _ := fserrors.Lookup(retErr)
		ev.Error, ev.Exit = entry.Code, entry.Exit
		if deliberateChoice(retErr) {
			ev.Exit = 0
		}
	}
	if !isText {
		ev.FilesSaved, ev.FilesSame, ev.FilesKept = ptrInt(len(files)), ptrInt(skippedSame), ptrInt(kept)
		if !ui.sink {
			ev.Dir = ui.outDir
		}
	}
	jsonEmitDone(ev)
	return retErr
}

// Text longer than this many bytes or lines is asked about before it is
// printed to a terminal; anything shorter shows straight away.
const (
	shortTextBytes = 4000
	shortTextLines = 40
)

// showText decides whether a received text payload is printed. Short text
// on a terminal always is (promptAccept skipped the question for it); a
// long payload gets the question it skipped, now that its size is known.
func (ui *receiverUI) showText(text string) bool {
	if ui.f.yes || ui.f.quiet || !stdoutIsTTY() {
		return true
	}
	lines := strings.Count(text, "\n") + 1
	if len(text) <= shortTextBytes && lines <= shortTextLines {
		return true
	}
	q := fmt.Sprintf("Show the text (%s, %s)?", uxlog.HumanBytes(int64(len(text))), uxlog.CountNoun(lines, "line"))
	for {
		fmt.Fprintf(os.Stderr, "  %s [Y/n] ", uxlog.Prompt(q))
		line, eof, ok := readLineCtx(ui.ctx)
		if !ok || eof {
			if eof {
				fmt.Fprintln(os.Stderr)
			}
			return false
		}
		switch line {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(os.Stderr, "  Please answer y or n.")
	}
}

// errKeptByChoice is ErrTargetExists rendered as the user's decision (ℹ, no
// "use --overwrite" advice). finishReceive no longer returns it — choosing
// "skip" exits 0 — but renderError still recognises it.
var errKeptByChoice = fmt.Errorf("%w: kept by choice", fserrors.ErrTargetExists)

// readTextPayload lifts the text payload off disk (it was staged as a file)
// and removes the staging file.
func readTextPayload(files []string) (string, error) {
	if len(files) == 0 {
		return "", nil
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		return "", fmt.Errorf("%w: text payload: %v", fserrors.ErrReadFailed, err)
	}
	_ = os.Remove(files[0])
	return string(b), nil
}

func printTextPayload(text string) error {
	if text == "" {
		return nil
	}
	if term.IsTerminal(int(os.Stdout.Fd())) {
		// A terminal acts on control bytes; piped stdout must stay byte-exact.
		text = sanitizeTextPayload(text)
	}
	if _, err := os.Stdout.WriteString(text); err != nil {
		return err
	}
	if text[len(text)-1] != '\n' {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			fmt.Println()
		} else if sameSink(os.Stdout, os.Stderr) {
			// stdout is piped, so its bytes must stay exact — but when
			// stderr shares the sink (2>&1, CI logs) the summary line would
			// butt against the payload. Break the line on stderr instead.
			// When stderr is elsewhere (the terminal), this newline would
			// only render as a stray blank line before the summary.
			fmt.Fprintln(os.Stderr)
		}
	}
	return nil
}

// sameSink reports whether two files point at the same underlying sink
// (same dev+inode), e.g. stdout and stderr under 2>&1. Windows caveat:
// pipes carry no file IDs there, so two *distinct* pipes compare equal —
// worst case a stray line break on a separately-piped stderr log.
// Consoles vs pipes vs disk files still compare correctly (filetype and
// real file IDs), which covers the cases that matter.
func sameSink(a, b *os.File) bool {
	ai, err1 := a.Stat()
	bi, err2 := b.Stat()
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

// headline names what landed where for the summary line. Names and
// destinations are file paths — green, the opencode file-reference
// convention.
func (ui *receiverUI) headline(h *wire.SenderHello, files []string) string {
	if ui.sink {
		return "Received"
	}
	// One file landed under the name it was offered as (a single-file or
	// stream send): name the saved file itself, path and all.
	if len(files) == 1 && h != nil &&
		(h.Mode == wire.ModeStream || sanitizeForDisplay(h.DisplayName, 128) == filepath.Base(files[0])) {
		return "Saved " + uxlog.Subject(displayPath(files[0]))
	}
	name := ""
	if h != nil && h.Mode != wire.ModeStream {
		name = sanitizeForDisplay(h.DisplayName, 128)
	}
	dest := saveTargetLabel(ui.outDir)
	if name == "" {
		return "Saved to " + dest
	}
	return "Saved " + uxlog.Subject(name) + " to " + dest
}

// printRecvSummary renders the post-transfer outcome: the outcome line,
// the numbers, then the exceptions, details faint. Kept-back files make it
// a partial success: the ! glyph, matching the E013 exit that follows —
// unless the user chose to keep them at the prompt, which is their
// decision, not a warning (no glyph, matching renderError's
// errKeptByChoice).
func printRecvSummary(f *flags, headline string, total, moved int64, kept, skippedSame int, keptByChoice bool, elapsed time.Duration, path connpath.Info) {
	if f.quiet {
		return
	}
	glyph := uxlog.Check()
	switch {
	case kept > 0 && keptByChoice:
		glyph = uxlog.Info()
	case kept > 0:
		glyph = uxlog.Warn()
	}
	if headline == "" {
		headline = "Received"
	}
	// Nothing moved and nothing kept-back, but files were skipped as identical:
	// a re-send where everything was already up to date. Say so plainly instead
	// of "Saved … 0 B", which reads as if it did work.
	if total == 0 && moved == 0 && kept == 0 && skippedSame > 0 {
		fmt.Fprintln(os.Stderr, uxlog.Check(), "Already up to date")
		fmt.Fprintln(os.Stderr, uxlog.Detail(uxlog.Join(uxlog.CountNoun(skippedSame, "file")+" unchanged", path.Tag())))
		printUpdateNotice(f)
		return
	}
	fmt.Fprintln(os.Stderr, glyph, headline)
	fmt.Fprintln(os.Stderr, uxlog.Detail(summaryDetail(total, moved, "received", elapsed, path)))
	// No "(use --overwrite)" here: the remedy already appears once — the
	// upfront --yes warning or E013's action line — and after an explicit
	// "n" at the prompt it must not appear at all.
	var exceptions []string
	if skippedSame > 0 {
		exceptions = append(exceptions, fmt.Sprintf("%d up to date", skippedSame))
	}
	if kept > 0 {
		exceptions = append(exceptions, fmt.Sprintf("%d kept", kept))
	}
	if len(exceptions) > 0 {
		fmt.Fprintln(os.Stderr, uxlog.Detail(uxlog.Join(exceptions...)))
	}
	if kept == 0 && moved > 0 {
		uxlog.Notify("Received "+uxlog.HumanBytes(total), time.Since(runStart))
	}
	printUpdateNotice(f)
}
