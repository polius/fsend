package main

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"
	"unicode"

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

// runReceive executes the receive-side flow: a 300 ms mDNS query for a
// same-LAN sender, and on miss a fall-through to the pairing + relay
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

	// Bare --pass: hidden no-echo prompt. We're not the password's
	// author — we have to type what the sender configured — so there's
	// no point offering a random default here.
	if err := resolvePassword(f, false); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	// LAN discovery first. An animated spinner makes the 300 ms wait feel
	// alive instead of frozen — and the same spinner can swap message
	// without dropping a stale "Looking…" line on screen if we fall
	// through to the pairing server.
	var spin *uxlog.Spinner
	if !f.quiet {
		spin = uxlog.StartSpinner("Looking for sender on local network")
	}
	q, err := landisc.Query(ctx, c, 300*time.Millisecond)
	if err != nil {
		// LAN miss → try the pairing + relay path. Swap the spinner
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

	// First LAN dial sits outside the retry loop. If it fails before
	// pairing, mDNS responded (cached, stale, or racing the sender's
	// de-announce) but the sender's LAN listener is gone — almost
	// always because the sender's internet path won the pair race or
	// the LAN listener was never really up. Fall through to the
	// internet path instead of reporting E003, which used to mislead
	// users in single-receiver scenarios and produced a flaky
	// TestReceive_Overwrite under -race.
	first, err := quicconn.Dial(ctx, addr, c)
	if err != nil {
		if !f.quiet {
			fmt.Fprintln(os.Stderr, uxlog.Info(), "Local sender unreachable — falling back to server.")
		}
		var connSpin *uxlog.Spinner
		if !f.quiet {
			connSpin = uxlog.StartSpinner("Connecting via server")
		}
		cfg, _ := config.Load()
		return runReceiveOverInternet(ctx, f, c, cfg, connSpin)
	}

	closeProg, accept, progressFn, recvBytes := newReceiverProgress(f)
	defer closeProg()

	start := time.Now()
	printPath(f, connpath.FromLAN())

	// LAN succeeded once — keep a normal retry loop around the transfer
	// itself so a mid-stream drop can re-dial and resume from the
	// receiver's partial.
	current := first
	if err := retry.WithBackoff(ctx, retry.Options{OnRetry: retryNoticeFor(f)}, nil,
		func(attempt int) error {
			if current == nil {
				res, err := quicconn.Dial(ctx, addr, c)
				if err != nil {
					return fmt.Errorf("dialing sender: %w", err)
				}
				current = res
			}
			res := current
			current = nil
			defer res.Close()
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
		}); err != nil {
		return err
	}

	printRecvSummary(f, recvBytes(), time.Since(start))
	return nil
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
//
// Timing: the progress bar is constructed lazily on first byte (see
// newReceiverProgress), so this prompt fires before any bar exists —
// no risk of mpb rendering on top of the password input line.
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
	fmt.Fprintf(os.Stderr, "  Incoming from %s:\n", sanitizeRemote(h.Hostname))
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
