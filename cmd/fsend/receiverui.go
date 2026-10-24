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

	"golang.org/x/term"

	"github.com/polius/fsend/internal/atomicfile"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
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
	ui.bytesHint = int64(summary.BytesToRecv)
	ui.diffBytes = int64(summary.DifferingBytes)
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
	ui.mu.Unlock()
	return ui.promptAccept(h, summary)
}

// promptAccept renders the incoming-transfer block and asks whether to save.
func (ui *receiverUI) promptAccept(h wire.SenderHello, summary transfer.ClassifySummary) bool {
	f := ui.f
	if f.quiet {
		return f.yes
	}
	peer := sanitizeRemote(h.Hostname)
	pathChip := ""
	if ui.pathInfo.Kind != connpath.KindUnknown {
		pathChip = "  ·  " + ui.pathInfo.Chip()
	}
	arrow := ""
	if uxlog.ColorFor(os.Stderr) {
		arrow = uxlog.Brand("⇣") + "  "
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  %sIncoming from %s%s\n", arrow, peer, pathChip)
	fmt.Fprintln(os.Stderr)
	renderArtifact(os.Stderr, h, summary, ui.sink)
	fmt.Fprintln(os.Stderr)

	_, flagged := ui.flagPolicy()
	if f.yes {
		fmt.Fprintln(os.Stderr, uxlog.Info(), "Accepting (--yes)")
		// --yes answers the accept prompt, not the existing-files question —
		// differing files are kept and the exit will be E013. Scripts reading
		// --yes as "yes to everything" deserve to hear that now.
		if summary.Differing > 0 && !flagged {
			fmt.Fprintf(os.Stderr, "%s Keeping %s — pass --overwrite to replace, or --rename to keep both\n",
				uxlog.Warn(), uxlog.CountNoun(summary.Differing, "differing file"))
		}
		return true
	}
	// The question renders in the violet prompt accent: when the user
	// glances back at the terminal, every line asking for a decision is
	// the same colour. Built in parts so the bold destination's reset
	// doesn't cut the accent short.
	var question string
	switch {
	case ui.sink:
		question = uxlog.Prompt("Write to stdout?")
	case h.Mode == wire.ModeStream && h.IsText:
		question = uxlog.Prompt("Accept?")
	default:
		question = uxlog.Prompt("Save to ") + boldIfColor(saveTargetLabel(ui.outDir)) + uxlog.Prompt("?")
	}
	// Files that already exist: one question covers both the accept and what
	// to do with them, instead of a second prompt after "yes".
	if len(summary.Conflicts) > 0 && !flagged && !ui.sink {
		return ui.askWithConflicts(question, summary.Conflicts)
	}
	for {
		fmt.Fprintf(os.Stderr, "  %s [Y/n] ", question)
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
	fmt.Fprintf(os.Stderr, "  %s\n", uxlog.Prompt(fmt.Sprintf("%s here already %s and %s:",
		uxlog.CountNoun(len(conflicts), "file"), pick(len(conflicts) == 1, "exists", "exist"), differVerb(len(conflicts)))))
	shown := min(len(conflicts), preview)
	for i, c := range conflicts[:shown] {
		fmt.Fprintf(os.Stderr, "    %s\n", conflictLabel(c, names[i]))
	}
	if len(conflicts) > shown {
		fmt.Fprintf(os.Stderr, "    … and %d more — l lists all %d\n", len(conflicts)-shown, len(conflicts))
	}
	options := "[K]eep both · [r]eplace " + uxlog.Alert("(deletes yours)") + " · [s]kip them · [n]o"
	for {
		fmt.Fprintf(os.Stderr, "  %s  %s ", question, options)
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
			for i, c := range conflicts {
				fmt.Fprintf(os.Stderr, "    %s\n", conflictLabel(c, names[i]))
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

// compactAgo renders an age in the shortest honest unit — "30s", "4m",
// "3h", "2d" — for the overwrite row's tiebreaker.
func compactAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// conflictLabel renders one conflict, distinguishing type clashes from
// content differences. The row name is a file path — green. A "differs"
// row carries the size delta in diff semantics (opencode's diffRemoved /
// diffAdded roles) and, crucially, in plain text: the leading − and + carry
// the meaning even where colour doesn't, so the row never reads as a no-op
// for a colourblind user or on a 256-colour terminal. LocalSize is always
// populated for "differs" (the classification Lstat'ed the file to call it
// differing); type clashes have no meaningful byte pair, so they keep the
// kind tag.
func conflictLabel(c transfer.Conflict, name string) string {
	row := uxlog.Path(name)
	if c.Kind != "differs" {
		return fmt.Sprintf("%s  (%s)", row, c.Kind)
	}
	row = fmt.Sprintf("%s  %s → %s",
		row,
		uxlog.Removed("− "+uxlog.HumanBytes(c.LocalSize)),
		uxlog.Added("+ "+uxlog.HumanBytes(int64(c.IncomingSize))))
	// Two rounded "3.3 MB"s read as a no-op, so when the displayed sizes
	// collide, say what actually differs: the byte delta, or — when even the
	// bytes match, meaning the classification rested on mtime — the local
	// copy's age.
	if uxlog.HumanBytes(c.LocalSize) == uxlog.HumanBytes(int64(c.IncomingSize)) {
		switch d := int64(c.IncomingSize) - c.LocalSize; {
		case d != 0:
			row += fmt.Sprintf("  (%+d B)", d)
		case !c.LocalModTime.IsZero():
			row += "  (local copy modified " + compactAgo(time.Since(c.LocalModTime)) + " ago)"
		}
	}
	return row
}

func (ui *receiverUI) onResume(fileIndex uint32, offset, total uint64) {
	ui.mu.Lock()
	if offset > ui.prev[fileIndex] {
		d := int64(offset - ui.prev[fileIndex])
		ui.prev[fileIndex] = offset
		ui.skipped += d
		// bytesHint counts only bytes still to receive (classify deducts
		// the partial's aligned offset), but the bar's numerator counts the
		// resumed prefix too — grow the total by the same delta or the bar
		// reads past 100%. Both sides then show absolute-over-full-size,
		// matching the sender.
		ui.bytesHint += d
		if ui.bar == nil && !ui.f.quiet {
			ui.bar = uxlog.New(ui.bytesHint, ui.names != nil, ui.pathInfo.BarTag())
		} else {
			ui.bar.SetTotal(ui.bytesHint, false)
		}
		ui.bar.Add(d)
	}
	quiet := ui.f.quiet
	ui.mu.Unlock()
	if !quiet {
		uxlog.Println(fmt.Sprintf("  %s %s", uxlog.Info(), resumeNotice(offset, total)))
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
	}
	if ui.bar == nil && !ui.f.quiet {
		ui.bar = uxlog.New(ui.bytesHint, ui.names != nil, ui.pathInfo.BarTag())
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

// printCancelKeptHint tells a Ctrl-C'd receiver that its partial data
// survives and how the transfer resumes. E026 itself is context-free
// ("Cancelled.") — only the UI knows whether bytes had already landed.
// Self-service leads: the still-running sender re-registers the same
// code, so the receiver can resume without involving the other person.
func printCancelKeptHint(f *flags, ui *receiverUI) {
	if f.quiet {
		return
	}
	if _, moved := ui.bytes(); moved > 0 {
		// Via uxlog.Println: the aborted bar is still live here (ui.close
		// runs on a defer), and its next refresh frame would erase a raw
		// stderr write.
		uxlog.Println(fmt.Sprintf("%s Partial data kept in %s — run the same fsend <code> again to resume while the sender is still waiting.",
			uxlog.Info(), displayPath(transfer.PartialDir(ui.outDir))))
	}
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
	if notice == nil {
		return nil
	}
	return func(attempt int, wait time.Duration, lastErr error) {
		ui.spin.Stop()
		notice(attempt, wait, lastErr)
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
		if !jsonEnabled() {
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
				len(files), uxlog.CountNoun(len(files)+skippedSame+kept+len(skipped), "file"), uxlog.Path(displayPath(ui.outDir)))
		}
		printRecvSummary(f, headline, total, moved, kept, skippedSame, keptByChoice, elapsed, ui.pathInfo)
		printSkippedList(f, "were", skipped)
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
	name := ""
	if h != nil {
		if h.Mode == wire.ModeStream {
			if len(files) > 0 {
				name = filepath.Base(files[0])
			}
		} else {
			name = sanitizeForDisplay(h.DisplayName, 128)
		}
	}
	dest := uxlog.Path(displayPath(ui.outDir))
	if name == "" {
		return "Saved to " + dest
	}
	return "Saved " + uxlog.Path(name) + " to " + dest
}

// boldIfColor wraps s in bold green when stderr can render it — used for
// the destination in the accept prompt: a file path (green, the
// opencode file-reference convention) that is also the answer to "where
// will this actually land?" (bold). Plain on pipes/files.
func boldIfColor(s string) string {
	if !uxlog.ColorFor(os.Stderr) {
		return s
	}
	return uxlog.Bold(uxlog.Path(s))
}

// printRecvSummary renders the post-transfer outcome line. Kept-back files
// make it a partial success: warn glyph, matching the E013 exit that follows —
// unless the user chose to keep them at the prompt, which is their decision,
// not a warning (info glyph, matching renderError's errKeptByChoice).
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
		fmt.Fprintf(os.Stderr, "%s Already up to date  ·  %s unchanged  ·  %s\n",
			uxlog.Check(), uxlog.CountNoun(skippedSame, "file"), path.Tag())
		printUpdateNotice(f)
		return
	}
	clauses := ""
	if skippedSame > 0 {
		clauses += "  ·  " + uxlog.Good(uxlog.CountNoun(skippedSame, "file")+" up to date")
	}
	// No "(use --overwrite)" here: the remedy already appears once — the
	// upfront --yes warning or E013's action line — and after an explicit
	// "n" at the prompt it must not appear at all.
	if kept > 0 {
		clauses += "  ·  " + uxlog.Alert(uxlog.CountNoun(kept, "file")+" kept")
	}
	fmt.Fprintf(os.Stderr, "%s %s — %s%s\n", glyph, headline, summaryLine(total, moved, "received", elapsed, path), clauses)
	uxlog.Notify("Received " + uxlog.HumanBytes(total))
	printUpdateNotice(f)
}
