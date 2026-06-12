package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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

// runSend executes the send-side flow. The LAN listener (mDNS-announced
// QUIC port derived from the code) and the pairing-server session are
// started in parallel; whichever the receiver reaches first wins. See
// sendpair.go for the coordinator.
func runSend(f *flags, paths []string) error {
	errorRoleSender = true // renderError picks sender-side catalog wording
	if f.textArg != "" && len(paths) > 0 {
		return fmt.Errorf("%w: --text cannot be combined with file arguments", fserrors.ErrUsage)
	}
	if f.textArg == "" && len(paths) == 0 {
		return fmt.Errorf("%w: nothing to send (provide a file, a directory, or --text)", fserrors.ErrUsage)
	}
	// Receive-side flags silently dropped on a send mask swapped-argument
	// mistakes (`fsend file --out dir`); reject instead.
	for _, rf := range []struct {
		name string
		set  bool
	}{{"--out", f.outDir != ""}, {"--yes", f.yes}, {"--overwrite", f.overwrite}} {
		if rf.set {
			return fmt.Errorf("%w: %s is a receive-side flag and has no effect when sending", fserrors.ErrUsage, rf.name)
		}
	}

	// The signal handler must be installed before the --pass prompt so
	// Ctrl-C there cancels cleanly instead of hitting a blocking read.
	ctx, cancel := signalContext()
	defer cancel()

	// Bare --pass: suggest a random default the user can accept by
	// pressing Enter. Done before any network setup so the prompt
	// can't collide with the pairing-server spinner.
	if err := resolvePassword(ctx, f, true); err != nil {
		return err
	}

	items, kind, totalFiles, label, cleanupItems, err := collectItems(f, paths)
	if err != nil {
		return err
	}
	defer cleanupItems()

	c, err := code.Generate()
	if err != nil {
		return fmt.Errorf("generating code: %w", err)
	}

	// Load config before the artifact block: it can print the E016
	// corruption warning, which must not land on the spinner's line.
	cfg := loadConfig(f.quiet)

	// Print the artifact (the receive command) exactly once, here,
	// before any path is attempted. Both LAN and internet paths use the
	// same locally-generated code — LAN announces an argon2id-derived
	// name via mDNS, and the internet path registers the code's argon2id
	// slot with the pairing server (the raw code never leaves this
	// machine except via the user). The code shown here is final.
	// The returned spinner animates "Waiting for receiver" until the
	// pair coordinator stops it (on pair success or before printing an
	// intermediate notice).
	waitSpin := printSendArtifact(f, c, items, kind, totalFiles, label)

	// Both paths run in parallel from T+0. Whichever pairs first wins;
	// the loser is cancelled and torn down. See sendpair.go for the
	// coordinator and the failure-mode UX. There is no LAN-only "budget"
	// — the receiver only contacts the pairing server after its
	// 300 ms mDNS query misses, so same-LAN receivers always win the
	// race against the server path, and cross-network receivers don't
	// wait on any timer.
	return runSendParallel(ctx, f, items, kind, totalFiles, label, c, cfg, waitSpin)
}

// collectItems resolves CLI args into the SourceItem list the wire
// protocol expects, plus the TransferKind discriminator and (when an
// archive was built) a cleanup function the caller must call after the
// send finishes — successfully or not — to remove the temp tar.
//
// Cases:
//
//	--text                                 → synthetic SourceItem
//	"-"                                    → stdin (one synthetic SourceItem)
//	single regular file                    → single-file walk, no archive
//	multiple regular files                 → multi-file walk, no archive
//	any directory in the input set         → tar bundle, archive transfer
//
// Bundling directories into a tar is the default-and-only behavior: it
// gives us deterministic resume on huge trees (imohash works on the
// single tar file), it dodges the small-file overhead that hurts croc
// on multi-thousand-file directories, and the user surface is the same
// whether they send a file or a folder.
// The uint32 return is the user-facing file count to surface in the
// artifact block and wire HELLO; 0 means "use len(items) as-is" (which
// is correct for non-archive transfers). The string is a display label
// for the sender's "Sending …" line; "" means use the per-kind default.
func collectItems(f *flags, paths []string) ([]transfer.SourceItem, wire.TransferKind, uint32, string, func(), error) {
	noop := func() {}
	// --exclude is consulted only when bundling a directory; for text and
	// stdin (and, below, plain files) it would be silently ignored.
	if len(f.excludes) > 0 && (f.textArg != "" || (len(paths) == 1 && paths[0] == "-")) {
		return nil, 0, 0, "", noop, errExcludeMisuse()
	}
	if f.textArg != "" {
		return synthesizeText(f.textArg), wire.TransferText, 0, "", noop, nil
	}
	if len(paths) == 1 && paths[0] == "-" {
		items, err := synthesizeStdin()
		if err != nil {
			return nil, 0, 0, "", noop, err
		}
		return items, wire.TransferStdin, 0, "", noop, nil
	}

	hasDir, err := containsDirectory(paths)
	if err != nil {
		return nil, 0, 0, "", noop, err
	}
	if !hasDir && len(f.excludes) > 0 {
		return nil, 0, 0, "", noop, errExcludeMisuse()
	}
	if hasDir {
		items, numFiles, cleanup, err := buildArchiveItem(paths, f.excludes)
		if err != nil {
			return nil, 0, 0, "", noop, err
		}
		// "0 files" in the artifact line is honest but easy to miss; a
		// fat-fingered --exclude glob deserves a nudge before the sender
		// shares a code for an empty archive.
		if numFiles == 0 && !f.quiet {
			msg := "The directory is empty — sending an empty archive."
			if len(f.excludes) > 0 {
				msg = "Every file matched --exclude — sending an empty archive."
			}
			fmt.Fprintln(os.Stderr, uxlog.Warn(), msg)
		}
		// Pick a label the user recognises. Single input path → its
		// basename (the folder they typed). Multiple inputs → "N items"
		// so the display reflects the user's command, not the
		// internal tar wrapper name.
		label := ""
		if len(paths) == 1 {
			label = filepath.Base(paths[0]) + "/"
		} else {
			label = uxlog.CountNoun(len(paths), "item")
		}
		return items, wire.TransferDirectory, numFiles, label, cleanup, nil
	}

	items, err := transfer.Walk(paths)
	if err != nil {
		return nil, 0, 0, "", noop, mapLocalReadErr(err)
	}
	if len(paths) == 1 {
		// Surface the user-typed basename, not the walker's relative
		// path (which may include subdirs when the user passed a
		// directory-resolved file).
		return items, wire.TransferSingleFile, 0, filepath.Base(paths[0]), noop, nil
	}
	return items, wire.TransferMultiFile, 0, uxlog.CountNoun(len(paths), "file"), noop, nil
}

// errExcludeMisuse is returned wherever --exclude would be silently
// ignored: a receive, or a send with no directory to bundle.
func errExcludeMisuse() error {
	return fmt.Errorf("%w: --exclude only applies when sending a directory", fserrors.ErrUsage)
}

// mapLocalReadErr promotes a permission failure on a local source file
// to E010 ("check the file permissions"). Without this it falls into the
// E099 catchall, which tells users to file a bug for their own chmod.
func mapLocalReadErr(err error) error {
	if errors.Is(err, fs.ErrPermission) {
		return fmt.Errorf("%w: %v", fserrors.ErrReadFailed, err)
	}
	return err
}

// containsDirectory reports whether any of paths refers to a directory.
// A missing path is left for transfer.Walk / BuildArchive to surface
// with a precise error.
func containsDirectory(paths []string) (bool, error) {
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return false, fmt.Errorf("%w: %s", fserrors.ErrSourceNotFound, p)
			}
			// EACCES, ELOOP, ENOTDIR, … on an existing path: a routine local
			// problem (usually permissions), not an internal bug. Map to
			// E010 instead of letting it fall through to the E099 catchall.
			return false, fmt.Errorf("%w: %s: %v", fserrors.ErrReadFailed, p, err)
		}
		if st.IsDir() {
			return true, nil
		}
	}
	return false, nil
}

// buildArchiveItem packages the given input paths into a single tar on
// disk and returns a one-element SourceItem the rest of the pipeline
// can treat like any other file. The returned cleanup removes the
// temp file; callers should `defer cleanup()` immediately.
func buildArchiveItem(paths []string, excludes []string) ([]transfer.SourceItem, uint32, func(), error) {
	res, err := transfer.BuildArchive(paths, excludes)
	if err != nil {
		return nil, 0, func() {}, mapLocalReadErr(err)
	}
	cleanup := func() { _ = os.Remove(res.Path) }

	st, err := os.Stat(res.Path)
	if err != nil {
		cleanup()
		return nil, 0, func() {}, err
	}

	item := transfer.SourceItem{
		Info: wire.FileInfo{
			Index:        0,
			RelativePath: transfer.ArchiveName,
			Size:         uint64(res.Size),
			Mode:         0o644,
			ModTime:      st.ModTime().UnixNano(),
			Blake3Root:   res.Blake3Root,
			Resumable:    true,
		},
		AbsPath:   res.Path,
		Resumable: true,
	}
	return []transfer.SourceItem{item}, uint32(res.NumFiles), cleanup, nil
}

func synthesizeText(s string) []transfer.SourceItem {
	// A text item is delivered as a small "fsend-text-<rand>.txt" file.
	// For LAN MVP, hash is irrelevant (resume is disabled for synthetic
	// items); we leave Blake3Root zero — the receiver still verifies via
	// per-chunk hashes.
	name := "fsend-text-" + shortRand() + ".txt"
	return []transfer.SourceItem{
		{
			Info: wire.FileInfo{
				Index:        0,
				RelativePath: name,
				Size:         uint64(len(s)),
				Mode:         0o644,
				ModTime:      time.Now().UnixNano(),
				Resumable:    false,
			},
			Reader: strings.NewReader(s),
		},
	}
}

func synthesizeStdin() ([]transfer.SourceItem, error) {
	name := "fsend-stdin-" + shortRand()
	// Stream stdin directly to the wire: chunks are emitted as bytes
	// arrive and the EOF chunk is marked with FlagLastChunk. The wire
	// FileInfo carries Streaming=true so the receiver doesn't expect a
	// pre-declared Size. This lets `pg_dump | fsend`-style pipelines
	// run with a bounded memory footprint and live progress instead of
	// silently buffering the entire stream before the transfer starts.
	//
	// Blake3Root is left zero; per-chunk hashes still cover integrity.
	return []transfer.SourceItem{
		{
			Info: wire.FileInfo{
				Index:        0,
				RelativePath: name,
				Size:         0,
				Mode:         0o644,
				ModTime:      time.Now().UnixNano(),
				Resumable:    false,
				Streaming:    true,
			},
			Reader: os.Stdin,
		},
	}, nil
}

// shortRand returns an 8-char crypto-random alphanumeric string for
// synthetic filenames.
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

// printSendArtifact renders the receive-command block on stderr and
// starts an animated "Waiting for receiver" spinner. Callers must
// Stop the spinner before printing anything else to stderr.
//
// Output rules:
//   - --quiet: bare code on stdout, nothing on stderr, nil spinner.
func printSendArtifact(f *flags, c string, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, label string) *uxlog.Spinner {
	if f.quiet {
		// Pipeline-friendly: just the code on stdout. If stdout is gone
		// (broken pipe to head, etc.) there's nothing useful to do; the
		// caller will surface its own error from the transfer path.
		_, _ = fmt.Fprintln(os.Stdout, c)
		return nil
	}

	fmt.Fprintln(os.Stderr)
	switch kind {
	case wire.TransferSingleFile:
		fmt.Fprintf(os.Stderr, "  Sending %s  ·  %s\n",
			items[0].Info.RelativePath, uxlog.HumanBytes(int64(items[0].Info.Size)))
	case wire.TransferDirectory:
		// Archive transfers carry one SourceItem (the tar) whose Size is
		// the on-wire payload; totalFiles is the user-meaningful count
		// of files packed into it. label is the source folder name (or
		// "N items" for multi-path) — never the tar wrapper name.
		var total uint64
		for _, it := range items {
			total += it.Info.Size
		}
		fmt.Fprintf(os.Stderr, "  Sending %s  ·  %s  ·  %s\n",
			label, uxlog.CountNoun(int(totalFiles), "file"), uxlog.HumanBytes(int64(total)))
	case wire.TransferMultiFile:
		fmt.Fprintf(os.Stderr, "  Sending %s  ·  %s\n",
			uxlog.CountNoun(len(items), "file"), uxlog.HumanBytes(totalBytes(items)))
	case wire.TransferText:
		fmt.Fprintf(os.Stderr, "  Sending text  ·  %s\n",
			uxlog.HumanBytes(totalBytes(items)))
	case wire.TransferStdin:
		// Stdin is streamed: size isn't known until the producer EOFs.
		// Print a placeholder; the progress bar carries the live byte
		// count.
		fmt.Fprintln(os.Stderr, "  Sending stdin stream  ·  size unknown")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  On the other machine, run:")
	fmt.Fprintln(os.Stderr)
	// The command is the single artifact: pasted as a whole, or the
	// highlighted code dictated out of it. A separate "Share this code"
	// block would repeat the same code four lines apart.
	fmt.Fprintf(os.Stderr, "      fsend %s\n", uxlog.Code(c))
	fmt.Fprintln(os.Stderr)
	return uxlog.StartSpinner("Waiting for receiver")
}

// hasConsumableReader reports whether any item is backed by a one-shot
// reader (stdin, --text). Such items cannot be replayed: a retry would
// resend from wherever the reader was left, so retries must be disabled.
func hasConsumableReader(items []transfer.SourceItem) bool {
	for _, it := range items {
		if it.Reader != nil {
			return true
		}
	}
	return false
}

// totalBytes sums the payload bytes across items.
func totalBytes(items []transfer.SourceItem) int64 {
	var t int64
	for _, it := range items {
		t += int64(it.Info.Size)
	}
	return t
}

// newSenderProgress builds a per-file delta-accumulating ProgressFn that
// drives a single overall progress bar. Returns a close func the caller
// should defer (no-op when --quiet), the ProgressFn for transfer.Send,
// an onResume hook that announces a resumed file and books its on-disk
// prefix as skipped rather than moved, a stats getter the post-transfer
// summary uses (moved = bytes actually sent this run; skipped = resumed
// prefixes), and an onStreamingEOF hook that latches the bar to the real
// total when an unknown-size streaming item EOFs.
//
// Under --quiet the bar and the resume notice are suppressed but the
// counters still accumulate so summary rendering stays accurate without
// branching on the flag in two places.
func newSenderProgress(f *flags, items []transfer.SourceItem) (closeFn func(), progressFn func(fileIndex uint32, bytesSent uint64), onResume func(fileIndex uint32, offset, total uint64), stats func() (moved, skipped int64), onStreamingEOF func(fileIndex uint32, finalBytes uint64)) {
	prev := make(map[uint32]uint64)
	var moved, skipped int64
	// bar is nil under --quiet and until the first byte: an eager bar
	// would draw 0% while the receiver still sits at its accept prompt —
	// that window belongs to the "Waiting for them to accept" spinner.
	// Mirrors the receiver's lazy bar; uxlog renders nil-safe.
	var bar *uxlog.Progress
	ensureBar := func() {
		if bar == nil && !f.quiet {
			// For streaming items the up-front total is 0 (size unknown).
			// The bar renders an indeterminate progress; SetTotal is
			// called from onStreamingEOF once the producer EOFs.
			bar = uxlog.New(totalBytes(items))
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
			// Fast-forward the per-file counter past the receiver's
			// prefix so it isn't booked as moved; on a mid-run retry
			// prev already covers it (the bytes were sent by us).
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
		func(_ uint32, finalBytes uint64) {
			bar.SetTotal(int64(finalBytes), true)
		}
}

// resumeNotice renders the "Resuming from 141 MB (71%)" clause shared by
// both roles' resume lines.
func resumeNotice(offset, total uint64) string {
	s := "Resuming from " + uxlog.HumanBytes(int64(offset))
	if total > 0 {
		s += fmt.Sprintf(" (%d%%)", offset*100/total)
	}
	return s
}

// printSendSummary renders the post-transfer success line on the sender.
// The artifact name is already shown in printSendArtifact at session
// start, so the summary deliberately omits it — repeating reads as
// padding. The line scans size → time → rate → route.
//
// Suppressed under --quiet (the bare code on stdout is the contract;
// nothing on stderr).
func printSendSummary(f *flags, total, moved int64, elapsed time.Duration, path connpath.Info) {
	if f.quiet {
		return
	}
	parts := summaryParts(total, moved, "sent", elapsed, path)
	fmt.Fprintf(os.Stderr, "%s Sent  ·  %s\n", uxlog.Check(), strings.Join(parts, "  ·  "))
	printUpdateNotice(f)
}

// summaryParts builds the bytes/duration/rate/path sequence that both
// send and recv summaries share: the numbers cluster together and the
// route reads as the closing note. moved is the byte count actually
// transferred this run; on a resumed transfer it is below total, the
// size gains a "(X sent)"/"(X received)" clause (verb per role), and
// the rate is computed from moved so it reflects real throughput rather
// than crediting the resumed prefix. Rate is appended only when
// HumanRate has a meaningful answer (above the noise floor) so tiny
// transfers don't print a misleading "13 B/s" figure.
func summaryParts(total, moved int64, verb string, elapsed time.Duration, path connpath.Info) []string {
	size := uxlog.HumanBytes(total)
	if moved < total {
		size += " (" + uxlog.HumanBytes(moved) + " " + verb + ")"
	}
	parts := []string{
		size,
		uxlog.HumanDuration(elapsed),
	}
	if r := uxlog.HumanRate(moved, elapsed); r != "" {
		parts = append(parts, r)
	}
	return append(parts, path.Tag())
}

// displayPath renders an absolute filesystem path for display: a
// $HOME prefix collapses to "~". Used in the accept prompt and the
// receive summary so users see "~/test" instead of the full
// "/Users/<name>/test".
//
// Falls back to the original path on any error or when no home prefix
// applies.
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

// signalContext wires Ctrl-C / SIGTERM to ctx cancellation so transfers
// can be cleanly aborted. After the first signal it stops intercepting,
// so a second Ctrl-C reverts to the default disposition and terminates
// the process outright — a safety valve if graceful teardown ever hangs.
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
