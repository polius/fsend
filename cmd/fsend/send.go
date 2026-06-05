package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/polius/fsend/internal/clipboardx"
	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/retry"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
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
		return errors.New("--text cannot be combined with file arguments")
	}
	if f.textArg == "" && len(paths) == 0 {
		return errors.New("nothing to send (provide a file, a directory, or --text)")
	}

	items, kind, totalFiles, label, cleanupItems, err := collectItems(f, paths)
	if err != nil {
		return err
	}
	defer cleanupItems()

	// If --code was supplied explicitly, this is unambiguously a "send to
	// someone who already has the code" flow. Otherwise we generate.
	c := f.codeArg
	if c == "" {
		c, err = code.Generate()
		if err != nil {
			return fmt.Errorf("generating code: %w", err)
		}
	} else {
		if err := code.Validate(c); err != nil {
			return fserrors.ErrInvalidCodeFormat
		}
	}

	ctx, cancel := signalContext()
	defer cancel()

	// Strategy: try LAN first (mDNS + QUIC direct). If that fails to
	// produce a connection within a short window, fall back to
	// rendezvous + relay. With --code set, we skip LAN because the user
	// is targeting a specific code/receiver path.
	if err := runSendOverLAN(ctx, f, items, kind, totalFiles, label, c); err == nil {
		return nil
	} else if !errors.Is(err, errLANUnavailable) {
		return err
	}

	// LAN bailed out — but if the reason is that the user pressed Ctrl-C
	// (or sent SIGTERM), don't silently start the internet fallback. The
	// LAN Accept failure is indistinguishable from a "no receiver showed
	// up" timeout at this layer, so we use ctx.Err() to disambiguate.
	if err := ctx.Err(); err != nil {
		return err
	}

	// LAN path unavailable — go internet (ICE → relay).
	cfg, _ := config.Load()
	return runSendOverInternet(ctx, f, items, kind, totalFiles, label, cfg)
}

// errLANUnavailable signals that the LAN path could not be set up. The
// caller falls back to the rendezvous + relay path.
var errLANUnavailable = errors.New("LAN unavailable")

// runSendOverLAN is the path we've validated empirically: mDNS announce
// + QUIC listener on a deterministic port. We give it a short window to
// pair before bailing out to the internet path.
//
// Once paired, transient transfer errors are retried symmetrically with
// the receiver's LAN loop, on the same Listener (the underlying UDP
// socket and NAT mapping persist across attempts). The "no receiver
// showed up" case is *not* a retry trigger — it short-circuits with
// errLANUnavailable so the caller can fall through to the internet
// path, which is its own retry-aware orchestration.
func runSendOverLAN(ctx context.Context, f *flags, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, label, c string) error {
	port := landisc.PortForCode(c)
	listenAddr := ":" + strconv.Itoa(port)
	ln, err := quicconn.ListenAddr(listenAddr, c)
	if err != nil {
		// Port already taken or similar — let the relay path try.
		return errLANUnavailable
	}
	defer ln.Close()
	announceIP := landisc.PreferredLocalIP()
	mdnsConn, err := landisc.Announce(c, announceIP, port)
	if err != nil {
		return errLANUnavailable
	}
	defer mdnsConn.Close()

	printSendArtifact(f, c, items, kind, totalFiles, label)

	closeProg, progressFn := newSenderProgress(f, items)
	defer closeProg()

	// `paired` flips true once the first Accept succeeds. Before then,
	// an Accept timeout means "no LAN receiver" and we fall through to
	// internet (non-transient). After pairing, the same timeout means
	// "receiver dropped, hasn't reconnected yet" — let retry handle it.
	paired := false
	err = retry.WithBackoff(ctx, retry.Options{OnRetry: retryNoticeFor(f)},
		lanSendIsTransient,
		func(attempt int) error {
			return runSenderLANOneAttempt(ctx, ln, items, kind, totalFiles, f, progressFn, &paired)
		})
	if err != nil {
		return err
	}

	if !f.quiet {
		fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	}
	return nil
}

// runSenderLANOneAttempt is one Accept + transfer pass on the LAN
// listener. The first call uses the 60-second pair window (current
// spec timeout); retries use a shorter 15-second window since the
// receiver should be re-Dialing within its own backoff schedule.
//
// On Accept failure, the function distinguishes "no one yet" from
// "lost mid-transfer" via the paired flag; lanSendIsTransient then
// routes errLANUnavailable to the caller (so the internet fallback
// can run) instead of retrying it.
func runSenderLANOneAttempt(ctx context.Context, ln *quicconn.Listener, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, f *flags, progressFn func(uint32, uint64), paired *bool) error {
	budget := 15 * time.Second
	if !*paired {
		budget = 60 * time.Second
	}
	acceptCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	res, err := ln.Accept(acceptCtx)
	if err != nil {
		if !*paired {
			return errLANUnavailable
		}
		return fmt.Errorf("QUIC accept: %w", err)
	}
	defer res.Close()

	if !*paired {
		*paired = true
		printPath(f, connpath.FromLAN())
	}

	return transfer.Send(ctx, &res.Streams, transfer.SendOptions{
		Items:         items,
		Hostname:      hostnameOrDefault(f.hostname),
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TransferKind:  kind,
		TotalFiles:    totalFiles,
		Compress:      !f.noCompress,
		Password:      f.passArg,
		ProgressFn:    progressFn,
	})
}

// lanSendIsTransient is a thin wrapper around retry.IsTransient that
// short-circuits errLANUnavailable. We need this because the retry
// layer is generic and doesn't know about our "fall back to internet"
// sentinel — but we *do* want the rest of the transient catalog
// (idle-timeout, EOF, connect-failed) to drive retries on the LAN
// path the same way they do on the internet path.
func lanSendIsTransient(err error) bool {
	if errors.Is(err, errLANUnavailable) {
		return false
	}
	return retry.IsTransient(err)
}

// collectItems resolves CLI args into the SourceItem list the wire
// protocol expects, plus the TransferKind discriminator and (when an
// archive was built) a cleanup function the caller must call after the
// send finishes — successfully or not — to remove the temp tar.
//
// Cases:
//   --text                                 → synthetic SourceItem
//   "-"                                    → stdin (one synthetic SourceItem)
//   single regular file                    → single-file walk, no archive
//   multiple regular files                 → multi-file walk, no archive
//   any directory in the input set         → tar bundle, archive transfer
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
		return synthesizeStdin(), wire.TransferStdin, 0, "", noop, nil
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
		return items, wire.TransferSingleFile, 0, "", noop, nil
	}
	return items, wire.TransferMultiFile, 0, "", noop, nil
}

// containsDirectory reports whether any of paths refers to a directory.
// A missing path is left for transfer.Walk / BuildArchive to surface
// with a precise error.
func containsDirectory(paths []string) (bool, error) {
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				return false, fmt.Errorf("no such file or directory: %s", p)
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

func synthesizeStdin() []transfer.SourceItem {
	name := "fsend-stdin-" + shortRand()
	// The wire protocol's per-file size is known upfront — sendOneFile
	// uses Size to decide its chunk loop. Stdin must therefore be
	// drained into a buffer before the transfer starts. For v0.1.0 this
	// is acceptable; a future streaming-stdin path would need a
	// Size = 0 ⇒ "unknown" sentinel in the wire protocol.
	buf, err := io.ReadAll(os.Stdin)
	if err != nil {
		// Failures here surface as a CLI error before any wire activity.
		buf = nil
	}
	return []transfer.SourceItem{
		{
			Info: wire.FileInfo{
				Index:        0,
				RelativePath: name,
				Size:         uint64(len(buf)),
				Mode:         0o644,
				ModTime:      time.Now().UnixNano(),
				Resumable:    false,
			},
			Reader: bytes.NewReader(buf),
		},
	}
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
// per PROJECT_SPEC.md "Send-side terminal UX" state 1.
//
// Honors the locked output rules:
//   - --quiet: bare code on stdout, nothing on stderr
//   - --no-clipboard: skip the auto-copy (default: copy)
//   - "✓ Code copied to clipboard" is shown only when the copy succeeded
//     (a Linux box without xclip/xsel falls through to "not available")
func printSendArtifact(f *flags, c string, items []transfer.SourceItem, kind wire.TransferKind, totalFiles uint32, label string) {
	if f.quiet {
		// Pipeline-friendly: just the code on stdout.
		fmt.Fprintln(os.Stdout, c)
		return
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
		fmt.Fprintf(os.Stderr, "  Sending %d items\n", len(items))
	case wire.TransferText:
		fmt.Fprintln(os.Stderr, "  Sending text")
	case wire.TransferStdin:
		fmt.Fprintln(os.Stderr, "  Sending from stdin")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  ─────────────────────────────────────────────")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "      %s\n", c)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  On the other machine, run:")
	fmt.Fprintf(os.Stderr, "      fsend %s\n", c)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  ─────────────────────────────────────────────")
	fmt.Fprintln(os.Stderr)
	if !f.noClip {
		if clipboardx.Copy(c) {
			fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Code copied to clipboard")
		}
	}
	fmt.Fprintln(os.Stderr, marker("⠋", "[*]"), "Waiting for receiver…")
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
// should defer (no-op when --quiet) and the ProgressFn for transfer.Send.
//
// Returns (no-op closeFn, nil) when --quiet is set — callers pass nil
// for ProgressFn so transfer.Send doesn't try to call into it.
func newSenderProgress(f *flags, items []transfer.SourceItem) (func(), func(fileIndex uint32, bytesSent uint64)) {
	if f.quiet {
		return func() {}, nil
	}
	bar := uxlog.New(totalBytes(items))
	prev := make(map[uint32]uint64)
	return bar.Done, func(fileIndex uint32, bytesSent uint64) {
		d := bytesSent - prev[fileIndex]
		prev[fileIndex] = bytesSent
		bar.Add(int64(d))
	}
}

// newReceiverProgress is the receive-side counterpart. The bar can't be
// constructed until the HELLO arrives (that's when we learn TotalBytes),
// so the returned Accept callback materializes the bar on accept.
// uxlog.Progress methods are nil-safe, so the closed-over var can stay
// nil for the rejected-accept path.
func newReceiverProgress(f *flags) (closeFn func(), accept func(wire.SenderHello) bool, progressFn func(uint32, uint64)) {
	if f.quiet {
		return func() {}, func(h wire.SenderHello) bool { return promptAccept(f, h) }, nil
	}
	var bar *uxlog.Progress
	prev := make(map[uint32]uint64)
	accept = func(h wire.SenderHello) bool {
		ok := promptAccept(f, h)
		if ok {
			bar = uxlog.New(int64(h.TotalBytes))
		}
		return ok
	}
	progressFn = func(fileIndex uint32, bytesWritten uint64) {
		d := bytesWritten - prev[fileIndex]
		prev[fileIndex] = bytesWritten
		bar.Add(int64(d))
	}
	closeFn = func() { bar.Done() }
	return
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

