package main

import (
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

	mu           sync.Mutex
	hello        *wire.SenderHello
	files        []string
	names        map[uint32]string // index → sanitized name; multi-file only
	curFile      uint32            // last index labelled on the bar
	prev         map[uint32]uint64
	total        int64 // bytes received this run (excludes resumed prefixes)
	skipped      int64 // resumed prefixes already on disk
	skippedSame  int   // files skipped because they were identical
	kept         int   // differing/conflicting entries left untouched
	keptByChoice bool  // kept because the user answered the overwrite prompt with "n"
	bar          *uxlog.Progress
	firstByte    time.Time
	bytesHint    int64
	diffBytes    int64 // bytes that move only if overwrite is approved
	manifestErr  error // a --manifest write failure, surfaced after the transfer

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
		Overwrite:      ui.f.overwrite,
		Checksum:       ui.f.checksum,
		Accept:         ui.accept,
		Password:       ui.f.passArg,
		PromptPass:     receiverPasswordPrompt(ui.ctx, ui.f),
		ProgressFn:     ui.progress,
		OnResume:       ui.onResume,
		OnSkip:         ui.onSkip,
		OnFileDone:     ui.onFileDone,
		OnConflictKept: ui.onConflictKept,
	}
	if ui.sink {
		o.Sink = os.Stdout
	}
	if ui.f.manifest != "" {
		o.OnManifest = ui.onManifest
	}
	// Under --quiet or --yes the confirm stays nil: the engine then keeps
	// differing files (skip) instead of blocking on a prompt nobody answers.
	if !ui.f.quiet && !ui.f.yes {
		o.ConfirmOverwrite = ui.confirmOverwrite
	}
	return o
}

// accept records the HELLO, then runs the accept prompt with the breakdown.
func (ui *receiverUI) accept(h wire.SenderHello, summary transfer.ClassifySummary) bool {
	ui.mu.Lock()
	cp := h
	ui.hello = &cp
	ui.bytesHint = int64(summary.BytesToRecv)
	ui.diffBytes = int64(summary.DifferingBytes)
	// --overwrite pulls the differing files too, so size the bar for them
	// upfront. The prompt path instead bumps the hint in confirmOverwrite.
	if ui.f.overwrite {
		ui.bytesHint += ui.diffBytes
	}
	// Current-file chip data: multi-file only. Names are peer-supplied, so
	// sanitize here — the only place they enter the UI state.
	if h.Mode == wire.ModeFiles && len(summary.Files) > 1 {
		ui.names = make(map[uint32]string, len(summary.Files))
		for _, e := range summary.Files {
			ui.names[e.Index] = sanitizeForDisplay(e.RelativePath, 128)
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
		pathChip = uxlog.Dim("  ·  " + ui.pathInfo.Chip())
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  Incoming from %s%s\n", peer, pathChip)
	fmt.Fprintln(os.Stderr)
	renderArtifact(os.Stderr, h, summary, ui.sink)
	fmt.Fprintln(os.Stderr)

	if f.yes {
		fmt.Fprintln(os.Stderr, uxlog.Info(), "Accepting (--yes)")
		// --yes answers the accept prompt, not the overwrite one — differing
		// files are kept and the exit will be E013. Scripts reading --yes as
		// "yes to everything" deserve to hear that now, not at the summary.
		if summary.Differing > 0 && !f.overwrite {
			fmt.Fprintf(os.Stderr, "%s Keeping %s that would be overwritten — pass --overwrite to replace\n",
				uxlog.Warn(), uxlog.CountNoun(summary.Differing, "differing file"))
		}
		return true
	}
	question := "Save to " + saveTargetLabel(ui.outDir) + "?"
	switch {
	case ui.sink:
		question = "Write to stdout?"
	case h.Mode == wire.ModeStream && h.IsText:
		question = "Accept?"
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

// confirmOverwrite is the consolidated prompt for differing/conflicting
// entries. Returns true to overwrite all, false to keep all local copies.
// Only "n"/"no"/bare-enter take the keep default; a mistyped "yes" must
// re-prompt, not silently count as "no" — mirroring promptAccept. EOF
// keeps the local copies but says so.
func (ui *receiverUI) confirmOverwrite(conflicts []transfer.Conflict) bool {
	const preview = 5
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  %s %s from your local copies:\n",
		uxlog.CountNoun(len(conflicts), "file"), differVerb(len(conflicts)))
	shown := min(len(conflicts), preview)
	for _, c := range conflicts[:shown] {
		fmt.Fprintf(os.Stderr, "    %s\n", conflictLabel(c))
	}
	if more := len(conflicts) - shown; more > 0 {
		fmt.Fprintf(os.Stderr, "    %s\n", uxlog.Dim(fmt.Sprintf("… and %d more", more)))
	}
	for {
		fmt.Fprint(os.Stderr, "  Overwrite all? [y / N / l = list all] ")
		line, eof, ok := readLineCtx(ui.ctx)
		if !ok {
			return false
		}
		if eof {
			fmt.Fprintf(os.Stderr, "\n%s No input to answer the prompt — keeping your local copies.\n", uxlog.Info())
			return false
		}
		switch line {
		case "y", "yes":
			// These files weren't in the bar's initial total (they only move
			// on consent); add them now, before the bar is created on first
			// byte, so it doesn't read past 100%.
			ui.mu.Lock()
			ui.bytesHint += ui.diffBytes
			ui.mu.Unlock()
			return true
		case "", "n", "no":
			// An answered prompt is a choice, not a failure — finishReceive
			// reports it with errKeptByChoice instead of a bare E013.
			ui.mu.Lock()
			ui.keptByChoice = true
			ui.mu.Unlock()
			return false
		case "l", "list":
			for _, c := range conflicts {
				fmt.Fprintf(os.Stderr, "    %s\n", conflictLabel(c))
			}
			continue
		}
		fmt.Fprintln(os.Stderr, "  Please answer y or n (or l to list all).")
	}
}

// conflictLabel renders one conflict, distinguishing type clashes from
// content differences.
func conflictLabel(c transfer.Conflict) string {
	name := sanitizeForDisplay(c.RelativePath, 128)
	if c.Kind != "differs" {
		return fmt.Sprintf("%s  (%s)", name, c.Kind)
	}
	return name
}

func (ui *receiverUI) onResume(fileIndex uint32, offset, total uint64) {
	ui.mu.Lock()
	if offset > ui.prev[fileIndex] {
		d := int64(offset - ui.prev[fileIndex])
		ui.prev[fileIndex] = offset
		ui.skipped += d
		if ui.bar == nil && !ui.f.quiet {
			ui.bar = uxlog.New(ui.bytesHint, ui.names != nil)
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

// onManifest writes the post-transfer record (path,size,status) as CSV to the
// --manifest path. Fired only after a successful receive, so the file isn't
// left describing a transfer that failed.
func (ui *receiverUI) onManifest(entries []transfer.ManifestEntry) {
	f, err := os.Create(ui.f.manifest)
	if err != nil {
		ui.manifestErr = fmt.Errorf("%w: %v", fserrors.ErrManifestWriteFailed, err)
		return
	}
	// A flush-time write failure can surface only at Close; swallowing it
	// would evade E038.
	defer func() {
		if cerr := f.Close(); cerr != nil && ui.manifestErr == nil {
			ui.manifestErr = fmt.Errorf("%w: %s: %v", fserrors.ErrManifestWriteFailed, ui.f.manifest, cerr)
		}
	}()
	cw := csv.NewWriter(f)
	_ = cw.Write([]string{"path", "size", "status"})
	for _, e := range entries {
		_ = cw.Write([]string{e.RelativePath, strconv.FormatUint(e.Size, 10), e.Status})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		ui.manifestErr = fmt.Errorf("%w: %s: %v", fserrors.ErrManifestWriteFailed, ui.f.manifest, err)
	}
}

func (ui *receiverUI) progress(fileIndex uint32, bytesWritten uint64) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.firstByte.IsZero() {
		ui.firstByte = time.Now()
	}
	if ui.bar == nil && !ui.f.quiet {
		ui.bar = uxlog.New(ui.bytesHint, ui.names != nil)
	}
	if name, ok := ui.names[fileIndex]; ok && fileIndex != ui.curFile {
		ui.curFile = fileIndex
		ui.bar.SetLabel(name)
	}
	d := bytesWritten - ui.prev[fileIndex]
	ui.prev[fileIndex] = bytesWritten
	ui.total += int64(d)
	ui.bar.Add(int64(d))
}

func (ui *receiverUI) onFileDone(path string) {
	ui.mu.Lock()
	ui.files = append(ui.files, path)
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
func printCancelKeptHint(f *flags, ui *receiverUI) {
	if f.quiet {
		return
	}
	if _, moved := ui.bytes(); moved > 0 {
		fmt.Fprintf(os.Stderr, "%s Partial data kept — when the sender runs fsend again, the transfer resumes here.\n", uxlog.Info())
	}
}

func (ui *receiverUI) close() {
	ui.closeOnce.Do(func() {
		ui.mu.Lock()
		bar := ui.bar
		ui.mu.Unlock()
		if bar != nil {
			bar.Done()
		}
	})
}

// finishReceive runs the post-transfer epilogue. When differing files were
// kept (no consent) it returns E013 so scripts get a non-zero exit, after
// showing the summary; a --manifest write failure surfaces the same way.
func finishReceive(f *flags, ui *receiverUI, elapsed time.Duration) error {
	ui.close()

	ui.mu.Lock()
	h := ui.hello
	files := append([]string(nil), ui.files...)
	firstByte := ui.firstByte
	kept := ui.kept
	keptByChoice := ui.keptByChoice
	skippedSame := ui.skippedSame
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
		// With kept-back files the peer-supplied display name ("2 files") would
		// overcount what actually landed — say how many of the offer were written.
		if kept > 0 && !ui.sink {
			headline = fmt.Sprintf("Saved %d of %s to %s",
				len(files), uxlog.CountNoun(len(files)+skippedSame+kept, "file"), displayPath(ui.outDir))
		}
		printRecvSummary(f, headline, total, moved, kept, skippedSame, keptByChoice, elapsed, ui.pathInfo)
	}
	// manifestErr is set by onManifest, which runs on this goroutine before we
	// return, so no lock is needed. The transfer succeeded; the failure is only
	// that --manifest couldn't be written.
	var retErr error
	switch {
	case ui.manifestErr != nil:
		retErr = ui.manifestErr
	case kept > 0 && keptByChoice:
		retErr = errKeptByChoice
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

// errKeptByChoice is ErrTargetExists after the user answered "n" at the
// overwrite prompt: same E013 exit so scripts see the partial, but rendered
// as their decision (ℹ, no "use --overwrite" advice they just declined).
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
	if _, err := os.Stdout.WriteString(text); err != nil {
		return err
	}
	if text[len(text)-1] != '\n' {
		if term.IsTerminal(int(os.Stdout.Fd())) {
			fmt.Println()
		} else {
			// stdout is piped, so its bytes must stay exact — but when
			// stderr shares the sink (2>&1, CI logs) the summary line would
			// butt against the payload. Break the line on stderr instead.
			fmt.Fprintln(os.Stderr)
		}
	}
	return nil
}

// headline names what landed where for the summary line.
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
	dest := displayPath(ui.outDir)
	if name == "" {
		return "Saved to " + dest
	}
	return "Saved " + name + " to " + dest
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
	parts := summaryParts(total, moved, "received", elapsed, path)
	if skippedSame > 0 {
		parts = append(parts, fmt.Sprintf("%s up to date", uxlog.CountNoun(skippedSame, "file")))
	}
	// No "(use --overwrite)" here: the remedy already appears once — the
	// upfront --yes warning or E013's action line — and after an explicit
	// "n" at the prompt it must not appear at all.
	if kept > 0 {
		parts = append(parts, fmt.Sprintf("%s kept", uxlog.CountNoun(kept, "file")))
	}
	fmt.Fprintf(os.Stderr, "%s %s  ·  %s\n", glyph, headline, strings.Join(parts, "  ·  "))
	printUpdateNotice(f)
}
