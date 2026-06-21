package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
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
	errorRoleSender = true
	if f.textArg != "" && len(paths) > 0 {
		return fmt.Errorf("%w: --text cannot be combined with file arguments", fserrors.ErrUsage)
	}
	if f.textArg == "" && len(paths) == 0 {
		return fmt.Errorf("%w: nothing to send (provide a file, a directory, or --text)", fserrors.ErrUsage)
	}
	for _, rf := range []struct {
		name string
		set  bool
	}{{"--out", f.outDir != ""}, {"--yes", f.yes}, {"--overwrite", f.overwrite}, {"--dry-run", f.dryRun}, {"--checksum", f.checksum}} {
		if rf.set {
			return fmt.Errorf("%w: %s is a receive-side flag and has no effect when sending", fserrors.ErrUsage, rf.name)
		}
	}

	ctx, cancel := signalContext()
	defer cancel()

	if err := resolvePassword(ctx, f, true); err != nil {
		return err
	}

	plan, err := collectPlan(f, paths)
	if err != nil {
		return err
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
	if f.quiet {
		_, _ = fmt.Fprintln(os.Stdout, c)
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
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  On the other machine, run:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "      fsend %s\n", uxlog.Code(c))
	fmt.Fprintln(os.Stderr)
	return uxlog.StartSpinner("Waiting for receiver")
}

// newSenderProgress builds the progress callbacks driving a single overall
// bar. Returns close, progress, onResume, a stats getter (moved/skipped), and
// onStreamingEOF (latches the bar total once a stream EOFs).
func newSenderProgress(f *flags, plan *sendPlan) (closeFn func(), progressFn func(uint32, uint64), onResume func(uint32, uint64, uint64), stats func() (int64, int64), onStreamingEOF func(uint32, uint64)) {
	prev := make(map[uint32]uint64)
	var moved, skipped int64
	var bar *uxlog.Progress
	ensureBar := func() {
		if bar == nil && !f.quiet {
			bar = uxlog.New(int64(plan.totalBytes))
		}
	}
	return func() { bar.Done() },
		func(fi uint32, b uint64) {
			ensureBar()
			d := b - prev[fi]
			prev[fi] = b
			bar.Add(int64(d))
			moved += int64(d)
		},
		func(fi uint32, offset, total uint64) {
			if offset > prev[fi] {
				d := int64(offset - prev[fi])
				prev[fi] = offset
				skipped += d
				ensureBar()
				bar.Add(d)
			}
			if !f.quiet {
				uxlog.Println(fmt.Sprintf("  %s %s", uxlog.Info(), resumeNotice(offset, total)))
			}
		},
		func() (int64, int64) { return moved, skipped },
		func(_ uint32, finalBytes uint64) { bar.SetTotal(int64(finalBytes), true) }
}

// resumeNotice renders the "Resuming from 141 MB (71%)" clause.
func resumeNotice(offset, total uint64) string {
	s := "Resuming from " + uxlog.HumanBytes(int64(offset))
	if total > 0 {
		s += fmt.Sprintf(" (%d%%)", offset*100/total)
	}
	return s
}

// printSendSummary renders the post-transfer success line.
func printSendSummary(f *flags, total, moved int64, elapsed time.Duration, path connpath.Info) {
	if f.quiet {
		return
	}
	parts := summaryParts(total, moved, "sent", elapsed, path)
	fmt.Fprintf(os.Stderr, "%s Sent  ·  %s\n", uxlog.Check(), strings.Join(parts, "  ·  "))
	printUpdateNotice(f)
}

// summaryParts builds the bytes/duration/rate/path sequence shared by both
// summaries. moved below total adds a "(X sent)" clause and bases the rate on
// moved alone.
func summaryParts(total, moved int64, verb string, elapsed time.Duration, path connpath.Info) []string {
	size := uxlog.HumanBytes(total)
	if moved < total {
		size += " (" + uxlog.HumanBytes(moved) + " " + verb + ")"
	}
	parts := []string{size, uxlog.HumanDuration(elapsed)}
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
// reverts to the default disposition and terminates outright.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
		signal.Stop(ch)
	}()
	return ctx, cancel
}
