package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/wire"
)

// runSend executes the send-side flow.
//
// v0.1.0 LAN-only path:
//  1. Walk the input paths.
//  2. Generate (or accept) a code.
//  3. Compute the deterministic LAN port from the code.
//  4. Bind a QUIC listener on that port.
//  5. Announce via mDNS.
//  6. Accept the first incoming QUIC connection.
//  7. Run the transfer protocol.
func runSend(f *flags, paths []string) error {
	if f.textArg != "" && len(paths) > 0 {
		return fmt.Errorf("%w: --text cannot be combined with file arguments", fserrors.ErrUsage)
	}
	if f.textArg == "" && len(paths) == 0 {
		return fmt.Errorf("%w: nothing to send (provide a file, a directory, or --text)", fserrors.ErrUsage)
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
	// rendezvous server adopts it via the suggested-code field on Create.
	// The returned spinner animates "Waiting for receiver" until the
	// pair coordinator stops it (on pair success or before printing an
	// intermediate notice).
	waitSpin := printSendArtifact(f, c, items, kind, totalFiles, label)

	// Both paths run in parallel from T+0. Whichever pairs first wins;
	// the loser is cancelled and torn down. See sendpair.go for the
	// coordinator and the failure-mode UX. There is no LAN-only "budget"
	// — the receiver only contacts the rendezvous server after its
	// 300 ms mDNS query misses, so same-LAN receivers always win the
	// race against the server path, and cross-network receivers don't
	// wait on any timer.
	cfg, _ := config.Load()
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
			return false, err
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
	// pre-declared Size. This lets `pg_dump | fsend -`-style pipelines
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
// per PROJECT_SPEC.md "Send-side terminal UX" state 1, then starts an
// animated "Waiting for receiver" spinner and returns it. Callers must
// Stop the spinner before printing anything else to stderr.
//
// Honors the locked output rules:
//   - --quiet: bare code on stdout, nothing on stderr, nil spinner.
func printSendArtifact(f *flags, c string, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, label string) *uxlog.Spinner {
	if f.quiet {
		// Pipeline-friendly: just the code on stdout.
		fmt.Fprintln(os.Stdout, c)
		return nil
	}

	fmt.Fprintln(os.Stderr)
	switch kind {
	case wire.TransferSingleFile:
		fmt.Fprintf(os.Stderr, "  Sending %s  (%s)\n", items[0].Info.RelativePath, humanBytes(int64(items[0].Info.Size)))
	case wire.TransferDirectory:
		// Archive transfers carry one SourceItem (the tar) whose Size is
		// the on-wire payload; totalFiles is the user-meaningful count
		// of files packed into it. label is the source folder name (or
		// "N items" for multi-path) — never the tar wrapper name.
		var total uint64
		for _, it := range items {
			total += it.Info.Size
		}
		fmt.Fprintf(os.Stderr, "  Sending %s  (%d files, %s)\n", label, totalFiles, humanBytes(int64(total)))
	case wire.TransferMultiFile:
		fmt.Fprintf(os.Stderr, "  Sending %d items  (%s)\n", len(items), humanBytes(totalBytes(items)))
	case wire.TransferText:
		fmt.Fprintf(os.Stderr, "  Sending text  (%s)\n", humanBytes(totalBytes(items)))
	case wire.TransferStdin:
		// Stdin is streamed: size isn't known until the producer EOFs.
		// Print a placeholder; the progress bar carries the live byte
		// count.
		fmt.Fprintln(os.Stderr, "  Sending from stdin  (streaming)")
	}
	sep := separator()
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  "+sep)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "      %s\n", c)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  On the other machine, run:")
	fmt.Fprintf(os.Stderr, "      fsend %s\n", c)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  "+sep)
	fmt.Fprintln(os.Stderr)
	return uxlog.StartSpinner("Waiting for receiver")
}

// separator is a CLI-facing alias for uxlog.Separator so callers don't
// have to qualify it on every artifact line.
func separator() string { return uxlog.Separator() }

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

// newReceiverProgress is the receive-side counterpart. The bar can't be
// constructed until the HELLO arrives (that's when we learn TotalBytes),
// so the returned Accept callback materializes the bar on accept.
// uxlog.Progress methods are nil-safe, so the closed-over var can stay
// nil for the rejected-accept path.
func newReceiverProgress(f *flags) (closeFn func(), accept func(wire.SenderHello) bool, progressFn func(uint32, uint64), recvBytes func() int64) {
	prev := make(map[uint32]uint64)
	var total int64
	recvBytes = func() int64 { return total }

	if f.quiet {
		closeFn = func() {}
		accept = func(h wire.SenderHello) bool { return promptAccept(f, h) }
		progressFn = func(fi uint32, b uint64) {
			d := b - prev[fi]
			prev[fi] = b
			total += int64(d)
		}
		return
	}

	var bar *uxlog.Progress
	// streamingTotal is true when the sender's HELLO carried TotalBytes=0,
	// which today only happens for piped stdin. The bar then renders
	// without ETA/percentage; at close we latch it to the accumulated
	// count so the trailing " done" suffix prints instead of "aborted".
	var streamingTotal bool
	accept = func(h wire.SenderHello) bool {
		ok := promptAccept(f, h)
		if ok {
			bar = uxlog.New(int64(h.TotalBytes))
			streamingTotal = h.TotalBytes == 0
		}
		return ok
	}
	progressFn = func(fileIndex uint32, bytesWritten uint64) {
		d := bytesWritten - prev[fileIndex]
		prev[fileIndex] = bytesWritten
		bar.Add(int64(d))
		total += int64(d)
	}
	closeFn = func() {
		if streamingTotal && total > 0 {
			bar.SetTotal(total, true)
		}
		bar.Done()
	}
	return
}

// humanRate renders a bytes-per-second figure in compact form.
// Returns "" when the figure would be meaningless (zero bytes, or an
// elapsed window too small to measure) so callers can omit the
// trailing "(<rate>)" clause cleanly instead of printing a placeholder.
func humanRate(bytes int64, elapsed time.Duration) string {
	// Below ~100 ms the rate is dominated by handshake noise and reads
	// as nonsense ("4.2 GB/s for a 12 KB transfer"). Hide it.
	if elapsed < 100*time.Millisecond || bytes <= 0 {
		return ""
	}
	rate := float64(bytes) / elapsed.Seconds()
	return humanBytes(int64(rate)) + "/s"
}

// humanDuration renders elapsed in compact form. Sub-second durations
// show with milliseconds; longer durations switch to seconds with one
// decimal, then minutes-and-seconds, then hours-minutes-seconds.
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d < 10*time.Second {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		m := int(d / time.Minute)
		s := int((d % time.Minute) / time.Second)
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	return fmt.Sprintf("%dh%02dm%02ds", h, m, s)
}

// printSendSummary renders the post-transfer success line on the sender.
// Suppressed under --quiet (the bare code on stdout is the contract;
// nothing on stderr).
func printSendSummary(f *flags, bytes int64, elapsed time.Duration) {
	if f.quiet {
		return
	}
	fmt.Fprintf(os.Stderr, "%s Sent %s in %s%s\n",
		uxlog.Check(), humanBytes(bytes), humanDuration(elapsed), rateSuffix(bytes, elapsed))
}

// printRecvSummary is the receive-side counterpart. The bytes figure is
// the actual received payload, captured by the progress callback so the
// number reflects what's on disk (post-resume, this can be less than the
// sender's TotalBytes if a prefix was already present).
func printRecvSummary(f *flags, bytes int64, elapsed time.Duration) {
	if f.quiet {
		return
	}
	fmt.Fprintf(os.Stderr, "%s Received %s in %s%s\n",
		uxlog.Check(), humanBytes(bytes), humanDuration(elapsed), rateSuffix(bytes, elapsed))
}

// rateSuffix returns "  (<rate>)" when humanRate has a meaningful value,
// otherwise "". Lets the summary line read as e.g. "Sent 4.2 MB in 8.1s"
// without a placeholder dash when the transfer was too quick to time.
func rateSuffix(bytes int64, elapsed time.Duration) string {
	r := humanRate(bytes, elapsed)
	if r == "" {
		return ""
	}
	return "  (" + r + ")"
}

// humanBytes renders a byte count in compact form.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// signalContext wires Ctrl-C / SIGTERM to ctx cancellation so transfers
// can be cleanly aborted.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}
