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
	"strconv"
	"strings"
	"syscall"
	"time"

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
	}{{"--out", f.outDir != ""}, {"--yes", f.yes}, {"--overwrite", f.overwrite}, {"--checksum", f.checksum}, {"--manifest", f.manifest != ""}} {
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

	waitSpin := printSendArtifact(f, c, plan)
	return runSendParallel(ctx, f, plan, c, cfg, waitSpin)
}

// collectPlan resolves CLI args into a sendPlan. Directories are walked
// recursively into per-file entries (no tar); a single dir/file or multiple
// paths all flow through the one listing-based path.
func collectPlan(f *flags, paths []string) (*sendPlan, error) {
	streamMisuse := f.textArg != "" || (len(paths) == 1 && paths[0] == "-")
	if len(f.excludes) > 0 && streamMisuse {
		return nil, errExcludeMisuse()
	}

	if f.textArg != "" {
		name := "fsend-text-" + shortRand() + ".txt"
		return &sendPlan{
			mode: wire.ModeStream, isText: true, stream: strings.NewReader(f.textArg),
			displayName: name, label: "text", totalBytes: uint64(len(f.textArg)),
		}, nil
	}
	if len(paths) == 1 && paths[0] == "-" {
		return &sendPlan{
			mode: wire.ModeStream, stream: os.Stdin,
			displayName: "fsend-stdin-" + shortRand(), label: "stdin stream",
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

// shortRand returns an 8-char crypto-random alphanumeric string.
func shortRand() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000"
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
	fmt.Fprintln(os.Stderr)
	switch {
	case plan.mode == wire.ModeStream && plan.isText:
		fmt.Fprintf(os.Stderr, "  Sending text  ·  %s\n", uxlog.HumanBytes(int64(plan.totalBytes)))
	case plan.mode == wire.ModeStream:
		fmt.Fprintln(os.Stderr, "  Sending stdin stream  ·  size unknown")
	default:
		name := ""
		if plan.label != "" {
			name = plan.label + "  ·  "
		}
		fmt.Fprintf(os.Stderr, "  Sending %s%s  ·  %s\n",
			name, uxlog.CountNoun(plan.totalFiles, "file"), uxlog.HumanBytes(int64(plan.totalBytes)))
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
	fmt.Fprintln(os.Stderr, "  On the other machine, run:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "      fsend %s\n", uxlog.Code(c))
	fmt.Fprintln(os.Stderr)
	return uxlog.StartSpinner("Waiting for receiver")
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
func newSenderProgress(f *flags, plan *sendPlan) (closeFn func(), progressFn func(uint32, uint64), onResume func(uint32, uint64, uint64), onSkip func(uint32, bool), stats func() senderStats, onStreamingEOF func(uint32, uint64), resetCounts func()) {
	prev := make(map[uint32]uint64)
	var s senderStats
	var bar *uxlog.Progress
	// Current-file chip: multi-file transfers only — a single file's name is
	// already in the pre-transfer block. Names are our own local paths, so no
	// display sanitization is needed (matches senderPreview).
	var names map[uint32]string
	if plan.mode == wire.ModeFiles && plan.totalFiles > 1 {
		names = make(map[uint32]string, len(plan.sources))
		for _, src := range plan.sources {
			if src.Entry.Type != wire.EntryDir {
				names[src.Entry.Index] = src.Entry.RelativePath
			}
		}
	}
	curFile := ^uint32(0)
	setLabel := func(fi uint32) {
		if names == nil || fi == curFile {
			return
		}
		curFile = fi
		bar.SetLabel(names[fi])
	}
	ensureBar := func() {
		if bar == nil && !f.quiet {
			bar = uxlog.New(int64(plan.totalBytes), names != nil)
		}
	}
	return func() { bar.Done() },
		func(fi uint32, b uint64) {
			ensureBar()
			setLabel(fi)
			d := b - prev[fi]
			prev[fi] = b
			bar.Add(int64(d))
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

// printSendSummary renders the post-transfer outcome line. Files the receiver
// kept (differing, no --overwrite there) make it a warning, not a success —
// the sender must not read "✓ Sent" when nothing was delivered. Partial skips
// without the kept flag stay the neutral "skipped": an old receiver doesn't
// report the distinction (wire.Decision.Kept), so "up to date" would overclaim.
func printSendSummary(f *flags, total int64, s senderStats, elapsed time.Duration, path connpath.Info) {
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
	glyph, headline := uxlog.Check(), "Sent"
	parts := summaryParts(total, s.moved, "sent", elapsed, path)
	if s.keptFiles > 0 {
		glyph = uxlog.Warn()
		if s.moved == 0 {
			// "Sent · 2.1 MB (0 B sent)" would contradict itself; name the
			// outcome and drop the redundant moved clause.
			headline = "Nothing sent"
			parts[0] = uxlog.HumanBytes(total) + " offered"
		}
	}
	if n := s.skippedFiles - s.keptFiles; n > 0 {
		parts = append(parts, uxlog.CountNoun(n, "file")+" skipped")
	}
	if s.keptFiles > 0 {
		parts = append(parts, uxlog.CountNoun(s.keptFiles, "file")+" kept by receiver (needs --overwrite there)")
	}
	fmt.Fprintf(os.Stderr, "%s %s  ·  %s\n", glyph, headline, strings.Join(parts, "  ·  "))
	printUpdateNotice(f)
}

// summaryParts builds the bytes/duration/rate/path sequence shared by both
// summaries. moved below total adds a "(X sent)" clause and bases the rate on
// moved alone.
func summaryParts(total, moved int64, verb string, elapsed time.Duration, path connpath.Info) []string {
	// A stream's total is unknown up front (0); by summary time the moved
	// count is the size — "Sent · 0 B" for a 15 MB pipe would be a lie.
	if moved > total {
		total = moved
	}
	size := uxlog.HumanBytes(total)
	if moved < total {
		size += " (" + uxlog.HumanBytes(moved) + " " + verb + ")"
	}
	parts := []string{size}
	// With zero bytes moved, elapsed is prompt dwell or connection wall
	// time, not a transfer duration — "0 B · 5.8s" reads as a slow
	// transfer. Omit it (HumanRate already suppresses the rate).
	if moved > 0 {
		parts = append(parts, uxlog.HumanDuration(elapsed))
	}
	if r := uxlog.HumanRate(moved, elapsed); r != "" {
		parts = append(parts, r)
	}
	return append(parts, path.Tag())
}

// displayPath renders an absolute path with $HOME collapsed to "~".
func displayPath(p string) string {
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
// (session delete, QUIC close) can take a few seconds, so the first
// signal says so — otherwise the pause reads as a hang.
func signalContext(quiet bool) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		// Notice before cancel(), and via uxlog.Println: a live progress
		// bar's next refresh frame would erase a raw stderr write, and
		// cancel() races the bar's teardown.
		if !quiet {
			uxlog.Println(fmt.Sprintf("%s Cancelling — press Ctrl-C again to force quit.", uxlog.Info()))
		}
		cancel()
		signal.Stop(ch)
	}()
	return ctx, cancel
}
