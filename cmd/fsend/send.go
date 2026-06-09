package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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
	if f.textArg != "" && len(paths) > 0 {
		return fmt.Errorf("%w: --text cannot be combined with file arguments", fserrors.ErrUsage)
	}
	if f.textArg == "" && len(paths) == 0 {
		return fmt.Errorf("%w: nothing to send (provide a file, a directory, or --text)", fserrors.ErrUsage)
	}

	// Bare --pass: suggest a random default the user can accept by
	// pressing Enter. Done before any network setup so the prompt
	// can't collide with the pairing-server spinner.
	if err := resolvePassword(f, true); err != nil {
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

	ctx, cancel := signalContext()
	defer cancel()

	// Print the artifact (code + receive command) exactly once, here,
	// before any path is attempted. Both LAN and internet paths use the
	// same locally-generated code — LAN announces it via mDNS, and the
	// pairing server adopts it via the suggested-code field on Create.
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
	cfg := loadConfig(f.quiet)
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
	if hasDir {
		items, numFiles, cleanup, err := buildArchiveItem(paths, f.excludes)
		if err != nil {
			return nil, 0, 0, "", noop, err
		}
		// Pick a label the user recognises. Single input path → its
		// basename (the folder they typed). Multiple inputs → "N items"
		// so the display reflects the user's command, not the
		// internal tar wrapper name.
		label := ""
		if len(paths) == 1 {
			label = filepath.Base(paths[0]) + "/"
		} else {
			label = fmt.Sprintf("%d items", len(paths))
		}
		return items, wire.TransferDirectory, numFiles, label, cleanup, nil
	}

	items, err := transfer.Walk(paths)
	if err != nil {
		return nil, 0, 0, "", noop, err
	}
	if len(paths) == 1 {
		// Surface the user-typed basename, not the walker's relative
		// path (which may include subdirs when the user passed a
		// directory-resolved file).
		return items, wire.TransferSingleFile, 0, filepath.Base(paths[0]), noop, nil
	}
	return items, wire.TransferMultiFile, 0, fmt.Sprintf("%d items", len(paths)), noop, nil
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
		return nil, 0, func() {}, err
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

// printSendArtifact renders the "code + receive command" block on stderr
// and starts an animated "Waiting for receiver" spinner. Callers must
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
		fmt.Fprintf(os.Stderr, "  Sending  %s  ·  %s\n",
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
		fmt.Fprintf(os.Stderr, "  Sending  %s  ·  %d files  ·  %s\n",
			label, totalFiles, uxlog.HumanBytes(int64(total)))
	case wire.TransferMultiFile:
		fmt.Fprintf(os.Stderr, "  Sending  %d items  ·  %s\n",
			len(items), uxlog.HumanBytes(totalBytes(items)))
	case wire.TransferText:
		fmt.Fprintf(os.Stderr, "  Sending  text  ·  %s\n",
			uxlog.HumanBytes(totalBytes(items)))
	case wire.TransferStdin:
		// Stdin is streamed: size isn't known until the producer EOFs.
		// Print a placeholder; the progress bar carries the live byte
		// count.
		fmt.Fprintln(os.Stderr, "  Sending  stdin stream  ·  size unknown")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Share this code:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "      %s\n", uxlog.Code(c))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  On the other machine, run:")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "      fsend %s\n", c)
	fmt.Fprintln(os.Stderr)
	return uxlog.StartSpinner("Waiting for receiver")
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
// a sentBytes getter that callers use to render the post-transfer summary
// — reflects actual bytes moved, which matters under resume —, and an
// onStreamingEOF hook that latches the bar to the real total when an
// unknown-size streaming item EOFs.
//
// Under --quiet the bar is suppressed but sentBytes still accumulates so
// summary rendering (in non-quiet callers) stays accurate without
// branching on the flag in two places.
func newSenderProgress(f *flags, items []transfer.SourceItem) (closeFn func(), progressFn func(fileIndex uint32, bytesSent uint64), sentBytes func() int64, onStreamingEOF func(fileIndex uint32, finalBytes uint64)) {
	prev := make(map[uint32]uint64)
	var total int64
	if f.quiet {
		return func() {},
			func(fi uint32, b uint64) {
				d := b - prev[fi]
				prev[fi] = b
				total += int64(d)
			},
			func() int64 { return total },
			func(uint32, uint64) {}
	}
	// For streaming items the up-front total is 0 (size unknown). The
	// bar renders an indeterminate progress; SetTotal is called from
	// onStreamingEOF once the producer EOFs.
	bar := uxlog.New(totalBytes(items))
	return bar.Done,
		func(fi uint32, b uint64) {
			d := b - prev[fi]
			prev[fi] = b
			bar.Add(int64(d))
			total += int64(d)
		},
		func() int64 { return total },
		func(_ uint32, finalBytes uint64) {
			bar.SetTotal(int64(finalBytes), true)
		}
}

// newReceiverProgress is the receive-side counterpart. The bar is
// materialized lazily on the first ProgressFn call rather than at
// Accept time. This matters because:
//
//   - Between Accept returning true and the first chunk arriving, the
//     transfer engine runs the password handshake (when the sender used
//     --pass). The handshake calls PromptPass, which writes a prompt to
//     stderr and reads a line back. If mpb were already drawing the bar
//     at Accept time, its 10 Hz repaint goroutine would step on the
//     password prompt line.
//   - Deferring also means a rejected transfer (Accept false, wrong
//     password, sender abort before any chunk) leaves no half-rendered
//     bar on screen — closeFn is a no-op if the bar was never created.
func newReceiverProgress(ctx context.Context, f *flags, outDir string, pathInfo connpath.Info) (
	closeFn func(),
	accept func(wire.SenderHello) bool,
	confirmOverwrite func(relPath string, existing int64, incoming uint64) bool,
	progressFn func(uint32, uint64),
	recvBytes func() int64,
) {
	prev := make(map[uint32]uint64)
	var total int64
	recvBytes = func() int64 { return total }

	// preApproved fires when promptAccept already surfaced a single-file
	// collision chip and the user said yes — the second "Overwrite? y/N"
	// question would just be friction. Captured here so both callbacks
	// share the flag without a parameter on RecvOptions.
	var preApproved bool

	if f.quiet {
		closeFn = func() {}
		accept = func(h wire.SenderHello) bool { return promptAccept(ctx, f, h, outDir, pathInfo) }
		progressFn = func(fi uint32, b uint64) {
			d := b - prev[fi]
			prev[fi] = b
			total += int64(d)
		}
		// confirmOverwrite stays nil under --quiet: the engine then
		// rejects with E013 rather than blocking on a prompt nobody
		// will see.
		return
	}

	var bar *uxlog.Progress
	var totalBytesHint int64
	// streamingTotal is true when the sender's HELLO carried TotalBytes=0,
	// which today only happens for piped stdin. The bar then renders
	// without ETA/percentage; at close we latch it to the accumulated
	// count so the trailing " done" suffix prints instead of "aborted".
	var streamingTotal bool
	accept = func(h wire.SenderHello) bool {
		ok := promptAccept(ctx, f, h, outDir, pathInfo)
		if !ok {
			return false
		}
		totalBytesHint = int64(h.TotalBytes)
		streamingTotal = h.TotalBytes == 0
		// Mirror the renderArtifact collision check: if the prompt
		// already disclosed an overwrite, treat the user's yes as
		// consent for the per-file confirm too.
		if h.TransferKind == wire.TransferSingleFile && !f.overwrite {
			target := filepath.Join(outDir, h.DisplayName)
			if st, err := os.Stat(target); err == nil && !st.IsDir() {
				preApproved = true
			}
		}
		return true
	}
	progressFn = func(fileIndex uint32, bytesWritten uint64) {
		if bar == nil {
			bar = uxlog.New(totalBytesHint)
		}
		d := bytesWritten - prev[fileIndex]
		prev[fileIndex] = bytesWritten
		bar.Add(int64(d))
		total += int64(d)
	}
	// Idempotent: callers can flush the bar explicitly before printing
	// the summary, then leave the deferred call as a no-op safety net.
	// Without that, stdin transfers (streamingTotal=true) print the
	// summary above the bar's terminal frame.
	var closeOnce sync.Once
	closeFn = func() {
		closeOnce.Do(func() {
			if bar == nil {
				return
			}
			if streamingTotal && total > 0 {
				bar.SetTotal(total, true)
			}
			bar.Done()
		})
	}
	if !f.yes {
		confirmOverwrite = func(relPath string, existing int64, incoming uint64) bool {
			if preApproved {
				return true
			}
			fmt.Fprintln(os.Stderr)
			fmt.Fprintf(os.Stderr, "  %s already exists  ·  local %s  ·  incoming %s\n",
				relPath, uxlog.HumanBytes(existing), uxlog.HumanBytes(int64(incoming)))
			fmt.Fprint(os.Stderr, "  Overwrite? [y/N] ")
			// Decline on Ctrl-C; the caller maps a cancelled ctx to E026.
			line, ok := readLineCtx(ctx)
			if !ok {
				return false
			}
			switch line {
			case "y", "yes":
				return true
			default:
				return false
			}
		}
	}
	return
}

// printSendSummary renders the post-transfer success line on the sender.
// The artifact name is already shown in printSendArtifact at session
// start, so the summary deliberately omits it — repeating reads as
// padding. Path tag goes last so the line scans size → time → route.
//
// Suppressed under --quiet (the bare code on stdout is the contract;
// nothing on stderr).
func printSendSummary(f *flags, bytes int64, elapsed time.Duration, path connpath.Info) {
	if f.quiet {
		return
	}
	parts := summaryParts(bytes, elapsed, path)
	fmt.Fprintf(os.Stderr, "%s Sent  ·  %s\n", uxlog.Check(), strings.Join(parts, "  ·  "))
}

// printRecvSummary is the receive-side counterpart. The bytes figure is
// the actual received payload, captured by the progress callback so the
// number reflects what's on disk (post-resume, this can be less than the
// sender's TotalBytes if a prefix was already present).
//
// destLabel is the human-readable save location ("~/Downloads" or an
// absolute path). When empty (text/stdin sinks, --quiet) the line
// collapses to "Received".
func printRecvSummary(f *flags, destLabel string, bytes int64, elapsed time.Duration, path connpath.Info) {
	if f.quiet {
		return
	}
	parts := summaryParts(bytes, elapsed, path)
	if destLabel != "" {
		fmt.Fprintf(os.Stderr, "%s Saved to %s  ·  %s\n",
			uxlog.Check(), destLabel, strings.Join(parts, "  ·  "))
		return
	}
	fmt.Fprintf(os.Stderr, "%s Received  ·  %s\n", uxlog.Check(), strings.Join(parts, "  ·  "))
}

// summaryParts builds the bytes/duration/path/rate sequence that both
// send and recv summaries share. Rate is appended only when humanRate
// has a meaningful answer (above the noise floor) so tiny transfers
// don't print a misleading "13 B/s" figure.
func summaryParts(bytes int64, elapsed time.Duration, path connpath.Info) []string {
	parts := []string{
		uxlog.HumanBytes(bytes),
		uxlog.HumanDuration(elapsed),
		path.Tag(),
	}
	if r := uxlog.HumanRate(bytes, elapsed); r != "" {
		parts = append(parts, r)
	}
	return parts
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
