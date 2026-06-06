package main

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

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
	"unicode"
)

// runReceive executes the receive-side flow: a 300 ms mDNS query for a
// same-LAN sender, and on miss a fall-through to the rendezvous + relay
// path in runReceiveOverInternet.
func runReceive(f *flags, c string) error {
	if err := code.Validate(c); err != nil {
		return fserrors.ErrInvalidCodeFormat
	}
	// --quiet suppresses the accept prompt. Without --yes there's no way
	// for the user to answer it, and the receive engine would silently
	// decline. Fail fast with a clear hint instead.
	if f.quiet && !f.yes {
		return fmt.Errorf("%w: --quiet on receive requires --yes (no prompt to answer otherwise)", fserrors.ErrUsage)
	}

	ctx, cancel := signalContext()
	defer cancel()

	// LAN discovery first. An animated spinner makes the 300 ms wait feel
	// alive instead of frozen — and the same spinner can swap message
	// without dropping a stale "Looking…" line on screen if we fall
	// through to the rendezvous server.
	var spin *uxlog.Spinner
	if !f.quiet {
		spin = uxlog.StartSpinner("Looking for sender on local network")
	}
	q, err := landisc.Query(ctx, c, 300*time.Millisecond)
	if err != nil {
		// LAN miss → try the rendezvous + relay path. Swap the spinner
		// message without leaving the first line as a static artifact.
		// runReceiveOverInternet owns the spinner's lifetime from here —
		// it stops it once Join lands.
		if spin != nil {
			spin.Stop()
			spin = uxlog.StartSpinner("Not on local network — connecting via server")
		}
		cfg, _ := config.Load()
		return runReceiveOverInternet(ctx, f, c, cfg, spin)
	}
	spin.Stop()

	addr := q.IP.String() + ":" + strconv.Itoa(q.Port)
	if f.debug && !f.quiet {
		fmt.Fprintln(os.Stderr, "    sender address:", addr)
	}

	hostname := f.hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	outDir := f.outDir
	if outDir == "" {
		outDir, err = os.Getwd()
		if err != nil {
			return err
		}
	}

	closeProg, accept, progressFn, recvBytes := newReceiverProgress(f)
	defer closeProg()

	start := time.Now()
	// Same retry shape as the internet receive path: a transient QUIC
	// or transfer error rebuilds the QUIC connection (Dial opens a
	// fresh UDP socket on each attempt) and the receiver's partial
	// file + imohash let the resumed transfer pick up where it left
	// off.
	paired := false
	if err := retry.WithBackoff(ctx, retry.Options{OnRetry: retryNoticeFor(f)}, nil,
		func(attempt int) error {
			return runReceiverLANOneAttempt(ctx, addr, c, outDir, hostname, f, accept, progressFn, &paired)
		}); err != nil {
		return err
	}

	printRecvSummary(f, recvBytes(), time.Since(start))
	return nil
}

// runReceiverLANOneAttempt is one Dial + transfer pass. The `paired`
// flag suppresses the "✓ direct (local)" line on retries so the user
// sees it exactly once even when we reconnect mid-session.
func runReceiverLANOneAttempt(ctx context.Context, addr, code, outDir, hostname string, f *flags, accept func(wire.SenderHello) bool, progressFn func(uint32, uint64), paired *bool) error {
	res, err := quicconn.Dial(ctx, addr, code)
	if err != nil {
		// mDNS told us the sender exists but the dial failed before
		// we ever paired — overwhelmingly this means the sender
		// already accepted another receiver and is no longer
		// Accept()ing on this listener. (The sender de-announces
		// mDNS at pair-time, but a receiver query can race the
		// announce going down.) Surface this as the friendly
		// "code already claimed" error rather than the raw QUIC
		// timeout, which the catalog catches as a generic E099.
		//
		// After pairing, dial failures are real transient issues
		// (the receiver lost the connection and is reconnecting via
		// the retry loop) — let those bubble up so retry can handle
		// them.
		if !*paired {
			return fmt.Errorf("%w (lan: %v)", fserrors.ErrCodeAlreadyClaimed, err)
		}
		return fmt.Errorf("dialing sender: %w", err)
	}
	defer res.Close()

	if !*paired {
		*paired = true
		printPath(f, connpath.FromLAN())
	}

	return transfer.Recv(ctx, &res.Streams, transfer.RecvOptions{
		Hostname:      hostname,
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TargetDir:     outDir,
		Overwrite:     f.overwrite,
		Accept:        accept,
		Password:      f.passArg,
		PromptPass:    receiverPasswordPrompt(f),
		ProgressFn:    progressFn,
	})
}

// receiverPasswordPrompt returns a callback that reads a password from
// stdin when the sender requires --pass but the receiver didn't supply
// one via --pass / FSEND_PASS. Returns nil under --quiet so the transfer
// engine immediately fails with ErrWrongPassword instead of blocking on
// a prompt that nobody will see.
//
// Input is read with no echo when stdin is a TTY (golang.org/x/term).
// Non-interactive callers should pass --pass or FSEND_PASS so the
// prompt never fires.
func receiverPasswordPrompt(f *flags) func() (string, error) {
	if f.quiet {
		return nil
	}
	return func() (string, error) {
		fmt.Fprintln(os.Stderr)
		return readPasswordHidden("  Password required by sender: ")
	}
}

func promptAccept(f *flags, h wire.SenderHello) bool {
	// --quiet: no prompt block at all. --yes is required (we don't
	// interactively prompt without UX).
	if f.quiet {
		return f.yes
	}
	pwChip := ""
	if h.HasPassword {
		pwChip = "  🔒 password required"
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Receiving from", sanitizeRemote(h.Hostname))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  "+separator())
	fmt.Fprintln(os.Stderr)
	switch h.TransferKind {
	case wire.TransferSingleFile:
		name := h.DisplayName
		if name == "" {
			name = "file"
		}
		fmt.Fprintf(os.Stderr, "      %s  (%s)%s\n", name, humanBytes(int64(h.TotalBytes)), pwChip)
	case wire.TransferDirectory:
		name := h.DisplayName
		if name == "" {
			name = "directory"
		}
		fmt.Fprintf(os.Stderr, "      %s  (%d files, %s)%s\n", name, h.TotalFiles, humanBytes(int64(h.TotalBytes)), pwChip)
	case wire.TransferMultiFile:
		fmt.Fprintf(os.Stderr, "      %d items  (%s)%s\n", h.TotalFiles, humanBytes(int64(h.TotalBytes)), pwChip)
	case wire.TransferText:
		fmt.Fprintf(os.Stderr, "      a piece of text  (%s)%s\n", humanBytes(int64(h.TotalBytes)), pwChip)
	case wire.TransferStdin:
		// Streamed stdin: sender's HELLO has TotalBytes=0 because the
		// size is only known at EOF on the producer side. Show "size
		// unknown" instead of "(0 B)".
		size := humanBytes(int64(h.TotalBytes))
		if h.TotalBytes == 0 {
			size = "size unknown"
		}
		fmt.Fprintf(os.Stderr, "      stream from stdin  (%s)%s\n", size, pwChip)
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  "+separator())
	fmt.Fprintln(os.Stderr)

	if f.yes {
		fmt.Fprintln(os.Stderr, uxlog.Check(), "Accepting (--yes)")
		return true
	}
	fmt.Fprintf(os.Stderr, "  Save to %s? [Y/n]: ", saveTargetLabel(f))
	switch readLine(os.Stdin) {
	case "n", "no":
		return false
	default:
		return true
	}
}

// saveTargetLabel renders the receive destination for the accept prompt.
// We don't expand "." to the absolute cwd here: most users live in their
// shell with relative paths, and an unexpanded "./report.pdf" reads as
// less surprising than "/Users/.../report.pdf". For an explicit --out we
// echo the user's path verbatim — same justification.
func saveTargetLabel(f *flags) string {
	if f.outDir == "" {
		return "current directory"
	}
	return f.outDir + "/"
}

// sanitizeRemote removes control characters, ANSI sequences, and Unicode
// format / bidirectional-override characters from peer-supplied strings
// before display. Without the bidi filter, a peer can render a misleading
// hostname using U+202E "RIGHT-TO-LEFT OVERRIDE" and friends, which is
// the textbook display-spoofing trick.
func sanitizeRemote(s string) string {
	const maxLen = 64
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r < 0x20, r == 0x7F:
			continue
		case isBidi(r):
			continue
		case unicode.Is(unicode.Cf, r): // any Unicode "Format" character
			continue
		}
		out = append(out, r)
		if len(out) >= maxLen {
			break
		}
	}
	if len(out) == 0 {
		// "peer" reads as a neutral placeholder instead of "(unknown)",
		// which can scan as "fsend failed to detect something". The
		// transfer is fine; we just don't have a hostname to display.
		return "peer"
	}
	return string(out)
}

// isBidi reports whether r is one of the bidirectional formatting
// codepoints that can be abused to spoof display order.
func isBidi(r rune) bool {
	switch {
	case r >= 0x202A && r <= 0x202E: // LRE, RLE, PDF, LRO, RLO
		return true
	case r >= 0x2066 && r <= 0x2069: // LRI, RLI, FSI, PDI
		return true
	case r == 0x200E || r == 0x200F: // LRM, RLM
		return true
	}
	return false
}
