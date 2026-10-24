package main

import (
	"context"
	"crypto/rand"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/wire"
)

// sendPlan is everything the transfer phase needs about what to send. It
// replaces the old (items, kind, totalFiles, label) tuple threaded through
// the pair coordinator.
type sendPlan struct {
	mode        wire.TransferMode
	sources     []transfer.Source // ModeFiles
	stream      io.Reader         // ModeStream
	isText      bool
	displayName string // peer-facing label (single filename, "myproj/", "N items")
	label       string // local UX label for the "Sending …" line
	totalFiles  int
	totalBytes  uint64
}

func (p *sendPlan) consumable() bool { return p.mode == wire.ModeStream }

// runSend executes the send-side flow.
func runSend(f *flags, paths []string) error {
	errorRole = "sender"
	if f.textArg != "" && len(paths) > 0 {
		return fmt.Errorf("%w: --text cannot be combined with positional arguments", fserrors.ErrUsage)
	}
	if f.textArg == "" && len(paths) == 0 {
		return fmt.Errorf("%w: nothing to send (provide a file, a directory, or --text)", fserrors.ErrUsage)
	}
	for _, rf := range []struct {
		name string
		set  bool
	}{{"--out", f.outDir != ""}, {"--yes", f.yes}, {"--overwrite", f.overwrite}, {"--rename", f.rename}, {"--checksum", f.checksum}, {"--manifest", f.manifest != ""}} {
		if rf.set {
			return fmt.Errorf("%w: %s is a receive-side flag and has no effect when sending", fserrors.ErrUsage, rf.name)
		}
	}

	ctx, cancel := signalContext(f.quiet)
	defer cancel()

	if err := resolvePassword(ctx, f, true); err != nil {
		return err
	}

	plan, err := collectPlan(f, paths)
	if err != nil {
		return err
	}

	// --preview lists what would be sent and stops — no code, no pairing.
	if f.preview {
		if plan.mode != wire.ModeFiles {
			return fmt.Errorf("%w: --preview only applies to file or folder sends", fserrors.ErrUsage)
		}
		if f.json {
			return fmt.Errorf("%w: --json does not apply to --preview (its CSV is already machine-readable)", fserrors.ErrUsage)
		}
		return writeSendPreview(os.Stdout, plan.sources)
	}

	c, err := code.Generate()
	if err != nil {
		return fmt.Errorf("generating code: %w", err)
	}
	cfg := loadConfig(f.quiet)
	startUpdateCheck(f)

	waitSpin := printSendArtifact(f, c, plan)
	return runSendParallel(ctx, f, plan, c, cfg, waitSpin)
}

// collectPlan resolves CLI args into a sendPlan. Directories are walked
// recursively into per-file entries (no tar); a single dir/file or multiple
// paths all flow through the one listing-based path.
func collectPlan(f *flags, paths []string) (*sendPlan, error) {
	stdinSend := len(paths) == 1 && paths[0] == "-"
	streamMisuse := f.textArg != "" || stdinSend
	if len(f.excludes) > 0 && streamMisuse {
		return nil, errExcludeMisuse()
	}
	if f.as != "" {
		// Files keep their own names; text is printed, not saved.
		if !stdinSend {
			return nil, fmt.Errorf("%w: --as names a piped stream (cat db.sql | fsend --as db.sql); files keep their own names", fserrors.ErrUsage)
		}
		if b := filepath.Base(f.as); b != f.as || b == "." || b == ".." || strings.ContainsAny(f.as, `/\`) {
			return nil, fmt.Errorf("%w: --as takes a file name, not a path: %q", fserrors.ErrUsage, f.as)
		}
	}

	if f.textArg != "" {
		name := "fsend-text-" + shortRand() + ".txt"
		return &sendPlan{
			mode: wire.ModeStream, isText: true, stream: strings.NewReader(f.textArg),
			displayName: name, label: "text", totalBytes: uint64(len(f.textArg)),
		}, nil
	}
	if stdinSend {
		name := f.as
		if name == "" {
			name = "fsend-stdin-" + shortRand()
		}
		return &sendPlan{
			mode: wire.ModeStream, stream: os.Stdin,
			displayName: name, label: "stdin stream",
		}, nil
	}

	hasDir, err := containsDirectory(paths)
	if err != nil {
		return nil, err
	}
	if !hasDir && len(f.excludes) > 0 {
		return nil, errExcludeMisuse()
	}

	sources, err := transfer.Walk(paths, f.excludes)
	if err != nil {
		return nil, mapLocalReadErr(err)
	}
	// Check the per-transfer entry cap now, before a code is issued, rather
	// than after the receiver has accepted.
	if len(sources) > transfer.MaxEntries {
		return nil, fmt.Errorf("%w: %d files and folders, the limit is %d", fserrors.ErrTooManyEntries, len(sources), transfer.MaxEntries)
	}
	plan := &sendPlan{
		mode:       wire.ModeFiles,
		sources:    sources,
		totalFiles: transfer.CountFiles(sources),
		totalBytes: transfer.TotalBytes(sources),
	}
	switch {
	case len(paths) == 1 && !transfer.IsContentsRef(paths[0]):
		// A single named file/dir arrives wrapped under its own name. Use the
		// resolved basename so it matches where files land (Walk roots at the
		// absolute base) — e.g. `fsend myproj` / `fsend ../myproj`.
		name := resolvedBase(paths[0])
		if hasDir {
			name += "/"
		}
		plan.displayName = name
		plan.label = name // shown as "Sending <name> · N files · …"
	default:
		// Multiple paths, or a contents send (`fsend .`): no single wrapper
		// name — describe it by count, like a multi-file transfer. label stays
		// empty so the "Sending" line reads "Sending N files · …".
		plan.displayName = uxlog.CountNoun(plan.totalFiles, "file")
	}
	// Nothing at all to send (empty folder, or everything excluded): fail fast
	// instead of generating a code and waiting on a receiver for a no-op.
	if len(sources) == 0 {
		msg := "nothing to send — the folder is empty"
		if len(f.excludes) > 0 {
			msg = "nothing to send — every file matched --exclude"
		}
		return nil, fmt.Errorf("%w: %s", fserrors.ErrUsage, msg)
	}
	return plan, nil
}

// resolvedBase returns the basename of p after resolving it to an absolute
// path, so "." and "foo/." and "foo/" all yield the real directory name (the
// same root Walk assigns). Falls back to the lexical base on error.
func resolvedBase(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return filepath.Base(abs)
	}
	return filepath.Base(p)
}

func errExcludeMisuse() error {
	return fmt.Errorf("%w: --exclude only applies when sending a directory", fserrors.ErrUsage)
}

func mapLocalReadErr(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %v", fserrors.ErrReadFailed, err)
	}
	return err
}

// containsDirectory reports whether any path is a directory, surfacing a
// precise error for a missing path.
func containsDirectory(paths []string) (bool, error) {
	hasDir := false
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				// Stat follows symlinks, so a dangling link lands here even
				// though the path exists. Leave it for Walk, which reports
				// E036 with the link's target — not a false "no such file".
				if _, lerr := os.Lstat(p); lerr == nil {
					continue
				}
				return false, fmt.Errorf("%w: %s", fserrors.ErrSourceNotFound, p)
			}
			return false, fmt.Errorf("%w: %s: %v", fserrors.ErrReadFailed, p, err)
		}
		if st.IsDir() {
			hasDir = true
		}
	}
	return hasDir, nil
}

// shortRand returns an 8-char crypto-random alphanumeric string used only to
// make default display names distinct.
func shortRand() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is effectively impossible, but the old constant
		// "00000000" would let two runs share a default name; a timestamp at
		// least keeps them distinct. Display-name only — never security-bearing.
		return fmt.Sprintf("%08x", uint32(time.Now().UnixNano()))
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}

// printSendArtifact renders the receive-command block and starts the
// "Waiting for receiver" spinner. --quiet emits just the code on stdout.
func printSendArtifact(f *flags, c string, plan *sendPlan) *uxlog.Spinner {
	// Under --json the code event replaces --quiet's bare-code line —
	// stdout must carry exactly one machine format.
	jsonEmitCode(c)
	if f.quiet {
		if !jsonEnabled() {
			_, _ = fmt.Fprintln(os.Stdout, c)
		}
		return nil
	}
	// Piped or redirected stdout (`fsend f | pbcopy`, `code=$(fsend f)`)
	// gets the bare code too, so scripts needn't know about --quiet. Not
	// when stdout and stderr share a sink (2>&1): the code is already there.
	if !jsonEnabled() && !stdoutIsTTY() && !sameSink(os.Stdout, os.Stderr) {
		_, _ = fmt.Fprintln(os.Stdout, c)
	}
	arrow := sendArrow()
	fmt.Fprintln(os.Stderr)
	switch {
	case plan.mode == wire.ModeStream && plan.isText:
		fmt.Fprintf(os.Stderr, "  %sSending text  ·  %s\n", arrow, uxlog.HumanBytes(int64(plan.totalBytes)))
	case plan.mode == wire.ModeStream && f.as != "":
		fmt.Fprintf(os.Stderr, "  %sSending stdin as %s  ·  size unknown\n", arrow, uxlog.Path(f.as))
	case plan.mode == wire.ModeStream:
		fmt.Fprintf(os.Stderr, "  %sSending stdin stream  ·  size unknown\n", arrow)
	default:
		name := ""
		if plan.label != "" {
			name = uxlog.Path(plan.label) + "  ·  "
		}
		fmt.Fprintf(os.Stderr, "  %sSending %s%s  ·  %s\n",
			arrow, name, uxlog.CountNoun(plan.totalFiles, "file"), uxlog.HumanBytes(int64(plan.totalBytes)))
		// Directory-only sends (an empty folder) survive collectPlan's
		// nothing-to-send guard because the dir entry itself is a source.
		// Sending it is legitimate — but "0 files" is usually a mistake,
		// so say what will actually happen.
		if plan.totalFiles == 0 {
			fmt.Fprintf(os.Stderr, "  %s No files here — only the empty directory will be created on the other side.\n", uxlog.Warn())
		}
		renderPreview(os.Stderr, senderPreview(plan.sources), 6)
	}
	fmt.Fprintln(os.Stderr)
	renderCodeBlock(os.Stderr, c, f.passArg != "")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  The code works for one receiver, while this stays open.")
	fmt.Fprintln(os.Stderr)
	return uxlog.StartSpinner("Waiting for receiver", uxlog.HintCancel)
}

// sendArrow renders the ⇢ direction marker prefixed to the send header
// on color-capable terminals; "" on pipes/files so the plain-text form
// scripts may scrape stays byte-stable. Brand orange: it marks the
// interactive hero flow.
func sendArrow() string {
	if !uxlog.ColorFor(os.Stderr) {
		return ""
	}
	return uxlog.Brand("⇢") + "  "
}

// codeBoxInner is the content width between the box borders. Both
// content lines are constants — the instruction sentence and the run
// command (codes are always 11 chars) — so a fixed box renders the same
// for every transfer; 34 leaves comfortable padding around the
// 28-character instruction row.
const codeBoxInner = 34

// codeBoxMinWidth is the narrowest terminal the box fits in: 2 leading
// spaces + 2 border columns + the 36-column interior. Below that (and on
// non-TTYs) the plain indented block renders instead — a wrapped box
// looks broken, and piped stderr must stay byte-stable anyway.
const codeBoxMinWidth = 42

// renderCodeBlock draws the "run this on the other machine" block. On a
// terminal wide enough to hold it, a light box frames the share code —
// the one thing on screen the user must read back or dictate — so it
// survives scrollback and busy terminals as a single visual object.
// The frame and the code share the brand orange (with the code bolded):
// the box is the hero, not structure to dim past. Fallback keeps the
// historical plain block for pipes, narrow terminals, and NO_COLOR.
func renderCodeBlock(w io.Writer, c string, password bool) {
	if !uxlog.ColorFor(w) || uxlog.TerminalWidth(0) < codeBoxMinWidth {
		_, _ = fmt.Fprintln(w, "  On the other machine, run:")
		_, _ = fmt.Fprintln(w)
		_, _ = fmt.Fprintf(w, "      fsend %s\n", uxlog.Code(c))
		if password {
			_, _ = fmt.Fprintln(w, "      (they'll need the password too)")
		}
		return
	}
	edge := uxlog.Brand("  │")
	row := func(content string, plainLen int) {
		// pad is computed off the plain length; content may carry ANSI
		// (the code) that must not count toward the width.
		_, _ = fmt.Fprintf(w, "%s %s%s %s\n", edge, content,
			strings.Repeat(" ", max(0, codeBoxInner-plainLen)), uxlog.Brand("│"))
	}
	_, _ = fmt.Fprintln(w, uxlog.Brand("  ┌"+strings.Repeat("─", codeBoxInner+2)+"┐"))
	row("  On the other machine, run:", len("  On the other machine, run:"))
	row("", 0)
	row("      "+uxlog.Accent("fsend")+" "+uxlog.Code(c), len("      fsend ")+len(c))
	if password {
		row("", 0)
		row("  "+uxlog.PasswordChip()+" too", utf8.RuneCountInString("  ⚠ password required too"))
	}
	_, _ = fmt.Fprintln(w, uxlog.Brand("  └"+strings.Repeat("─", codeBoxInner+2)+"┘"))
}

// senderPreview projects the walked sources into preview rows, dropping
// directory entries so the count matches the "N files" headline. Names are
// the user's own local paths, so no display sanitization is needed.
func senderPreview(sources []transfer.Source) []previewItem {
	items := make([]previewItem, 0, len(sources))
	for _, s := range sources {
		if s.Entry.Type == wire.EntryDir {
			continue
		}
		// Followed symlinks travel as regular files; annotate the row with the
		// origin link so the user sees it was a symlink — and so a size that
		// repeats (link + its target both in the set) reads as intentional.
		items = append(items, previewItem{
			name: s.Entry.RelativePath,
			size: s.Entry.Size,
			from: s.LinkTarget,
		})
	}
	return items
}

// writeSendPreview writes a CSV (path,size) of the files that would be sent,
// for --preview. Directories are structural and omitted so the rows match the
// "N files" count; encoding/csv quotes any path containing a comma or quote.
func writeSendPreview(w io.Writer, sources []transfer.Source) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"path", "size"})
	for _, s := range sources {
		if s.Entry.Type == wire.EntryDir {
			continue
		}
		_ = cw.Write([]string{s.Entry.RelativePath, strconv.FormatUint(s.Entry.Size, 10)})
	}
	cw.Flush()
	return cw.Error()
}

// senderStats reports a finished send's tallies. skippedFiles counts entries
// the receiver already had (declined via DecisionSkip) — the signal that
// distinguishes an "already up to date" no-op from a real transfer. The
// summary's size is the offered total (plan.totalBytes); moved is what
// actually crossed the wire, so a partial send reads "Y (X sent)".
type senderStats struct {
	moved        int64 // bytes pushed on the wire this run
	skippedFiles int   // whole files (non-dir) the receiver declined, keptFiles included
	keptFiles    int   // subset of skippedFiles the receiver kept as differing copies
}

// newSenderProgress builds the progress callbacks driving a single overall
// bar. Returns close, progress, onResume, onSkip, a stats getter, and
// onStreamingEOF (latches the bar total once a stream EOFs). All callbacks
// run on the single send-loop goroutine, so the counters need no locking.
// pathInfo feeds the bar's route chip ("local network"/"direct"/"relay").
func newSenderProgress(f *flags, plan *sendPlan, pathInfo connpath.Info) (closeFn func(), progressFn func(uint32, uint64), onResume func(uint32, uint64, uint64), onSkip func(uint32, bool), stats func() senderStats, onStreamingEOF func(uint32, uint64), resetCounts func()) {
	prev := make(map[uint32]uint64)
	var s senderStats
	var bar *uxlog.Progress
	// Current-file chip: multi-file transfers only — a single file's name is
	// already in the pre-transfer block. Names are our own local paths, so no
	// display sanitization is needed (matches senderPreview).
	var names map[uint32]string
	var ordinal map[uint32]int
	if plan.mode == wire.ModeFiles && plan.totalFiles > 1 {
		// Same projection the preview list uses, trimmed by the same shared
		// root the headline already names — otherwise every chip row repeats
		// "myproject/" and spends columns on the one part of the path that
		// never changes.
		items := senderPreview(plan.sources)
		stripCommonDir(items)
		names = make(map[uint32]string, len(items))
		ordinal = make(map[uint32]int, len(items))
		k := 0
		for _, src := range plan.sources {
			if src.Entry.Type == wire.EntryDir {
				continue
			}
			names[src.Entry.Index] = items[k].name
			ordinal[src.Entry.Index] = k + 1
			k++
		}
	}
	curFile := ^uint32(0)
	setLabel := func(fi uint32) {
		if names == nil || fi == curFile {
			return
		}
		curFile = fi
		bar.SetLabel(names[fi])
		// The "file i/N" counter is the honest progress signal for a folder
		// of many small files, where the byte bar barely moves.
		bar.SetFilePos(ordinal[fi], plan.totalFiles)
	}
	ensureBar := func() {
		if bar == nil && !f.quiet && wantsBar(int64(plan.totalBytes), plan.mode == wire.ModeStream && !plan.isText) {
			bar = uxlog.New(int64(plan.totalBytes), names != nil, pathInfo.BarTag())
		}
	}
	// pos is the bar's position (resumed prefixes included), for the
	// "stopped at 52%" line a dropped receiver gets.
	var pos int64
	advance := func(d int64) {
		pos += d
		if plan.totalBytes > 0 {
			sendState.setPct(int(min(100, pos*100/int64(plan.totalBytes))))
		}
	}
	return func() { bar.Done() },
		func(fi uint32, b uint64) {
			ensureBar()
			setLabel(fi)
			// Clamp: on a retry the sender can report a byte count below the
			// prior attempt's high-water mark (prev persists across attempts),
			// and unsigned subtraction would wrap into a huge negative bar.
			d := uint64(0)
			if b > prev[fi] {
				d = b - prev[fi]
			}
			prev[fi] = b
			bar.Add(int64(d))
			advance(int64(d))
			s.moved += int64(d)
		},
		func(fi uint32, offset, total uint64) {
			if offset > prev[fi] {
				// Reflect the resumed prefix in the bar so it shows true
				// progress; the summary's total comes from the offered size,
				// so no separate byte counter is needed here.
				d := int64(offset - prev[fi])
				prev[fi] = offset
				ensureBar()
				bar.Add(d)
				advance(d)
			}
			if !f.quiet {
				uxlog.Println(fmt.Sprintf("  %s %s", uxlog.Info(), resumeNotice(offset, total)))
			}
		},
		func(_ uint32, kept bool) {
			s.skippedFiles++
			if kept {
				s.keptFiles++
			}
		},
		func() senderStats { return s },
		func(_ uint32, finalBytes uint64) { bar.SetTotal(int64(finalBytes), true) },
		func() {
			// A retry reclassifies against the receiver and re-fires onSkip, so
			// the file tallies must start fresh; moved bytes are keyed per file
			// (prev map) and self-correct, so they persist.
			s.skippedFiles = 0
			s.keptFiles = 0
		}
}

// resumeNotice renders the "Resuming from 141 MB (71%)" clause.
func resumeNotice(offset, total uint64) string {
	s := "Resuming from " + uxlog.HumanBytes(int64(offset))
	if total > 0 {
		s += fmt.Sprintf(" (%d%%)", offset*100/total)
	}
	return s
}

// printSendSummary renders the post-transfer outcome line. subject names
// what was sent when there is a single path (plan.label) — rendered green
// like every file reference — so the summary mirrors the receiver's
// "Saved X to Y" shape. Files the receiver kept (differing, no
// --overwrite there) make it a warning, not a success — the sender must
// not read "✓ Sent" when nothing was delivered. Partial skips without the
// kept flag stay the neutral "skipped": an old receiver doesn't report
// the distinction (wire.Decision.Kept), so "up to date" would overclaim.
func printSendSummary(f *flags, subject string, total int64, s senderStats, elapsed time.Duration, path connpath.Info) {
	if f.quiet {
		return
	}
	// Everything skipped, nothing kept: the receiver already had it all and
	// says "Already up to date" — "Sent · (0 B sent)" would contradict
	// itself. Mirror the receiver's headline.
	if s.moved == 0 && s.keptFiles == 0 && s.skippedFiles > 0 {
		fmt.Fprintf(os.Stderr, "%s Already up to date  ·  %s unchanged  ·  %s\n",
			uxlog.Check(), uxlog.CountNoun(s.skippedFiles, "file"), path.Tag())
		printUpdateNotice(f)
		return
	}
	glyph, verb := uxlog.Check(), "Sent"
	if s.keptFiles > 0 {
		glyph = uxlog.Warn()
		if s.moved == 0 {
			// "Sent — 2.1 MB" would contradict itself; name the outcome and
			// mark the size as the offer, not what crossed the wire.
			verb = "Nothing sent"
		}
	}
	// "Sent report.pdf to bobs-laptop": the subject says what, the peer
	// says to whom — the last check that the code reached the right person.
	headline := verb
	if verb == "Sent" {
		if subject != "" {
			headline += " " + subject
		}
		if peer := sendState.peerName(); peer != "" {
			headline += " to " + peer
		}
	}
	clauses := ""
	if n := s.skippedFiles - s.keptFiles; n > 0 {
		clauses += "  ·  " + uxlog.Good(uxlog.CountNoun(n, "file")+" skipped")
	}
	if s.keptFiles > 0 {
		clauses += "  ·  " + uxlog.Alert(uxlog.CountNoun(s.keptFiles, "file")+" kept by receiver")
	}
	fmt.Fprintf(os.Stderr, "%s %s — %s%s\n", glyph, headline, summaryLine(total, s.moved, "sent", elapsed, path), clauses)
	if s.keptFiles == 0 && s.moved > 0 {
		uxlog.Notify("Sent "+uxlog.HumanBytes(max(total, s.moved)), time.Since(runStart))
	}
	printUpdateNotice(f)
}

// sendSubject names what a send carried, for the summary headline: the
// single path (green, a file reference), "3 files", "text" or "stdin".
func sendSubject(plan *sendPlan) string {
	switch {
	case plan.mode == wire.ModeStream && plan.isText:
		return "text"
	case plan.mode == wire.ModeStream:
		return "stdin"
	case plan.label != "":
		return uxlog.Path(plan.label)
	}
	return uxlog.CountNoun(plan.totalFiles, "file")
}

// wantsBar reports whether a transfer earns a progress bar: anything of a
// megabyte or more, and streams of unknown size. Smaller transfers finish
// before a bar says anything; the summary line covers them.
func wantsBar(total int64, unknownSize bool) bool {
	return unknownSize || total >= 1000*1000
}

// sendState is the sender's memory across re-pair rounds: who the
// receiver is and how far the last round got, for the lines that name
// them. Package state like errorRole: the transfer stack is long gone by
// the time some of those lines print.
var sendState sessionState

type sessionState struct {
	mu   sync.Mutex
	peer string // sanitized device name; "" until the first HELLO_ACK
	pct  int    // bar position of the latest round, 0..100
}

func (s *sessionState) setPeer(name string) (changed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed = name != s.peer
	s.peer = name
	return changed
}

func (s *sessionState) peerName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peer
}

func (s *sessionState) setPct(p int) {
	s.mu.Lock()
	s.pct = p
	s.mu.Unlock()
}

func (s *sessionState) percent() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pct
}

// onPeer is the sender's transfer.SendOptions.OnPeer: on an accepting
// receiver it prints "✓ bobs-laptop accepted" (once per receiver, so a
// retry round doesn't repeat it) — the sender's view of who connected.
func onPeer(f *flags, hostname string, accepted bool) {
	name := sanitizeRemote(hostname)
	if changed := sendState.setPeer(name); changed && accepted && !f.quiet {
		fmt.Fprintf(os.Stderr, "%s %s accepted\n", uxlog.Check(), name)
	}
}

// maxPasswordRounds caps the pairing rounds a password-protected send
// offers: each round allows transfer.PasswordAttempts tries, so a leaked
// code buys at most rounds×attempts online guesses.
const maxPasswordRounds = 3

// passwordMissNotice names a round that ended on the password.
func passwordMissNotice(err error) string {
	who := sendState.peerName()
	if who == "" {
		who = "the receiver"
	}
	if errors.Is(err, fserrors.ErrPasswordRequired) {
		return "No password from " + who
	}
	return "Wrong password from " + who
}

// dropNotice is the line a sender sees when its receiver goes away
// mid-transfer, naming who, how far it got and the code that still works.
func dropNotice(deliberate bool, code string) string {
	who := sendState.peerName()
	at := ""
	if p := sendState.percent(); p > 0 && p < 100 {
		at = fmt.Sprintf(" at %d%%", p)
	}
	again := uxlog.Code("fsend " + code)
	if deliberate {
		if who == "" {
			who = "The receiver"
		}
		return fmt.Sprintf("%s stopped%s — to resume, rerun on that device: %s", who, at, again)
	}
	if who == "" {
		who = "the receiver"
	}
	return fmt.Sprintf("Lost contact with %s%s — waiting; to resume, rerun on that device: %s", who, at, again)
}

// summaryLine renders the size/duration/rate/route tail shared by both
// summaries: "209.7 MB in 4.5s (46.1 MB/s)  ·  local network". The size
// stays in the default colour; elapsed, rate, and the route chip are
// dimmed — secondary detail after the sentence's subject. moved below
// total adds a "(X sent)" clause and bases the rate on moved alone.
func summaryLine(total, moved int64, verb string, elapsed time.Duration, path connpath.Info) string {
	// A stream's total is unknown up front (0); by summary time the moved
	// count is the size — "Sent — 0 B" for a 15 MB pipe would be a lie.
	if moved > total {
		total = moved
	}
	size := uxlog.HumanBytes(total)
	if moved < total && moved > 0 {
		size += " (" + uxlog.HumanBytes(moved) + " " + verb + ")"
	}
	line := size
	// With zero bytes moved, elapsed is prompt dwell or connection wall
	// time, not a transfer duration — "0 B · 5.8s" reads as a slow
	// transfer. Omit it (HumanRate already suppresses the rate).
	if moved > 0 {
		line += " in " + uxlog.HumanDuration(elapsed)
	}
	if r := uxlog.HumanRate(moved, elapsed); r != "" {
		line += " (" + r + ")"
	}
	return line + "  ·  " + path.Tag()
}

// displayPath renders an absolute path as short as it stays unambiguous:
// relative to the working directory ("." or "./sub/x"), else with $HOME
// collapsed to "~". Long absolute paths made prompts wrap.
func displayPath(p string) string {
	if wd, err := os.Getwd(); err == nil {
		if p == wd {
			return "."
		}
		if rel, err := filepath.Rel(wd, p); err == nil && filepath.IsLocal(rel) {
			return "." + string(filepath.Separator) + rel
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// signalContext wires Ctrl-C / SIGTERM to ctx cancellation; a second signal
// reverts to the default disposition and terminates outright. Teardown
// (session delete, QUIC close) usually ends within a second; only when it
// doesn't does the force-quit hint appear — otherwise the pause reads as a
// hang.
func signalContext(quiet bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	var mu sync.Mutex
	var hint *time.Timer
	done := false
	go func() {
		select {
		case <-ch:
		case <-ctx.Done():
			return // run finished without a signal
		}
		// Notice before cancel(), and via uxlog.Println: it clears a live
		// spinner's line first, and a live progress bar's next refresh
		// would erase a raw stderr write; cancel() races the bar's teardown.
		if !quiet {
			uxlog.Println(fmt.Sprintf("%s Cancelling…", uxlog.Info()))
			mu.Lock()
			if !done {
				hint = time.AfterFunc(time.Second, func() {
					uxlog.Println("  Still cleaning up — press Ctrl-C again to force quit.")
				})
			}
			mu.Unlock()
		}
		cancel()
		signal.Stop(ch)
	}()
	// The caller's cancel marks teardown done: release the signal and make
	// sure the hint never fires late.
	return ctx, func() {
		signal.Stop(ch)
		cancel()
		mu.Lock()
		done = true
		if hint != nil {
			hint.Stop()
		}
		mu.Unlock()
	}
}

// sourceIssueHooks returns the engine callbacks for files that could not be
// read (skipped, reported now and listed at the end) or changed while being
// read, plus an accessor for the skipped paths. Keyed by index, so a retry
// that hits the same file doesn't count it twice.
func sourceIssueHooks(f *flags, plan *sendPlan) (skipped func() []string, onSkipped func(uint32, error), onChanged func(uint32)) {
	var mu sync.Mutex
	paths := map[uint32]string{}
	name := func(i uint32) string {
		if int(i) < len(plan.sources) {
			return displayPath(plan.sources[i].AbsPath)
		}
		return fmt.Sprintf("#%d", i)
	}
	skipped = func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, 0, len(paths))
		for _, p := range paths {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
	onSkipped = func(i uint32, err error) {
		mu.Lock()
		paths[i] = name(i) + " — " + sourceErrReason(err)
		mu.Unlock()
		if !f.quiet {
			uxlog.Println(fmt.Sprintf("%s Skipped %s: %s", uxlog.Warn(), name(i), sourceErrReason(err)))
		}
	}
	onChanged = func(i uint32) {
		if !f.quiet {
			uxlog.Println(fmt.Sprintf("%s %s changed while it was being sent — the copy may be inconsistent", uxlog.Warn(), name(i)))
		}
	}
	return skipped, onSkipped, onChanged
}

// sourceErrReason is the cause of a source read failure without the path
// (which the caller already shows).
func sourceErrReason(err error) string {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// printSkippedList lists files a transfer skipped, after the summary.
func printSkippedList(f *flags, verb string, rows []string) {
	if f.quiet || len(rows) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "%s %s %s skipped:\n", uxlog.Warn(), uxlog.CountNoun(len(rows), "file"), verb)
	const show = 10
	for _, r := range rows[:min(len(rows), show)] {
		fmt.Fprintf(os.Stderr, "    %s\n", r)
	}
	if len(rows) > show {
		fmt.Fprintf(os.Stderr, "    … and %d more\n", len(rows)-show)
	}
}
