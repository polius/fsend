package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/retry"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
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
	// Sink mode writes no files, so --overwrite can only be a mistake.
	if f.outDir == "-" && f.overwrite {
		return fmt.Errorf("%w: --overwrite has no effect with --out -", fserrors.ErrUsage)
	}

	// Bare --pass: hidden no-echo prompt. We're not the password's
	// author — we have to type what the sender configured — so there's
	// no point offering a random default here.
	if err := resolvePassword(f, false); err != nil {
		return err
	}

	ctx, cancel := signalContext()
	defer cancel()

	// LAN discovery is a 300 ms probe — short enough that showing a
	// spinner only flashes a line that immediately gets cleared on miss,
	// which reads as glitchy. Stay silent through the probe; only show a
	// spinner once we know we're going down the longer internet path.
	q, err := landisc.Query(ctx, c, 300*time.Millisecond)
	if err != nil {
		// LAN miss → internet path. A single "Connecting" spinner runs
		// from here through Join + ICE/relay setup, replacing what used
		// to be a sequence of brief flashes. runReceiveOverInternet
		// owns its lifetime and stops it just before printPath.
		cfg := loadConfig(f.quiet)
		var spin *uxlog.Spinner
		if !f.quiet {
			spin = uxlog.StartSpinner("Connecting")
		}
		return runReceiveOverInternet(ctx, f, c, cfg, spin)
	}

	addr := q.IP.String() + ":" + strconv.Itoa(q.Port)
	if f.debug && !f.quiet {
		fmt.Fprintln(os.Stderr, "    sender address:", addr)
	}

	outDir, sink, err := resolveOutDir(f)
	if err != nil {
		return err
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
		// Debug-only: the transfer may still end up "Direct on local
		// network" via the server-paired race, so surfacing this notice
		// by default reads as the tool contradicting itself.
		if f.debug && !f.quiet {
			fmt.Fprintln(os.Stderr, uxlog.Info(), "Local sender unreachable — falling back to server.")
		}
		cfg := loadConfig(f.quiet)
		var connSpin *uxlog.Spinner
		if !f.quiet {
			connSpin = uxlog.StartSpinner("Connecting")
		}
		return runReceiveOverInternet(ctx, f, c, cfg, connSpin)
	}

	ui := newReceiverUI(ctx, f, outDir, sink, connpath.FromLAN())
	defer ui.close()

	start := time.Now()

	// LAN succeeded once — keep a normal retry loop around the transfer
	// itself so a mid-stream drop can re-dial and resume from the
	// receiver's partial. Sink mode gets one attempt: emitted bytes
	// can't be reconciled, so a retry would duplicate output.
	opts := retry.Options{OnRetry: retryNoticeFor(f)}
	if sink {
		opts.Attempts = 1
	}
	current := first
	if err := retry.WithBackoff(ctx, opts, nil,
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
			return transfer.Recv(ctx, &res.Streams, ui.recvOptions(hostnameOrDefault(f.hostname)))
		}); err != nil {
		// A Ctrl-C at an interactive prompt cancels ctx but surfaces as a
		// decline/target-exists error from the engine; report it as a
		// cancellation (E026) rather than that incidental error.
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}

	return finishReceive(f, ui, time.Since(start))
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
func receiverPasswordPrompt(ctx context.Context, f *flags) func() (string, error) {
	if f.quiet {
		return nil
	}
	return func() (string, error) {
		fmt.Fprintln(os.Stderr)
		return readPasswordHiddenCtx(ctx, "  Password required by sender: ")
	}
}

// renderArtifact prints the indented artifact line and (for a single
// file that would collide) the warning chip that pre-discloses the
// overwrite. The chip means the user only ever sees one question per
// transfer instead of an accept-then-overwrite double prompt.
func renderArtifact(w io.Writer, h wire.SenderHello, outDir string, f *flags) {
	pwChip := ""
	if h.HasPassword {
		pwChip = "  " + uxlog.PasswordChip()
	}
	switch h.TransferKind {
	case wire.TransferSingleFile:
		// DisplayName is peer-supplied; sanitize it like the hostname so a
		// crafted filename can't inject ANSI/bidi into the accept prompt.
		name := sanitizeForDisplay(h.DisplayName, 128)
		if name == "" {
			name = "file"
		}
		_, _ = fmt.Fprintf(w, "      %s  ·  %s%s\n", name, uxlog.HumanBytes(int64(h.TotalBytes)), pwChip)
		// outDir is "" in sink mode — nothing on disk to collide with.
		if !f.overwrite && outDir != "" {
			target := filepath.Join(outDir, name)
			if st, err := os.Stat(target); err == nil && !st.IsDir() {
				// Under --yes there is no accept prompt to consent
				// through, so the transfer will fail with E013.
				chip := fmt.Sprintf("already in %s (%s) — will be overwritten if you accept",
					displayPath(outDir), uxlog.HumanBytes(st.Size()))
				if f.yes {
					chip = fmt.Sprintf("already in %s (%s) — rerun with --overwrite to replace",
						displayPath(outDir), uxlog.HumanBytes(st.Size()))
				}
				_, _ = fmt.Fprintln(w, "      "+uxlog.Warn()+" "+uxlog.Dim(chip))
			}
		}
	case wire.TransferDirectory:
		name := sanitizeForDisplay(h.DisplayName, 128)
		if name == "" {
			name = "directory"
		}
		_, _ = fmt.Fprintf(w, "      %s  ·  %s  ·  %s%s\n",
			name, uxlog.CountNoun(int(h.TotalFiles), "file"), uxlog.HumanBytes(int64(h.TotalBytes)), pwChip)
	case wire.TransferMultiFile:
		_, _ = fmt.Fprintf(w, "      %s  ·  %s%s\n",
			uxlog.CountNoun(int(h.TotalFiles), "file"), uxlog.HumanBytes(int64(h.TotalBytes)), pwChip)
	case wire.TransferText:
		_, _ = fmt.Fprintf(w, "      text  ·  %s%s\n",
			uxlog.HumanBytes(int64(h.TotalBytes)), pwChip)
	case wire.TransferStdin:
		// Streamed stdin: sender's HELLO has TotalBytes=0 because the
		// size is only known at EOF on the producer side. Show "size
		// unknown" instead of "(0 B)".
		size := uxlog.HumanBytes(int64(h.TotalBytes))
		if h.TotalBytes == 0 {
			size = "size unknown"
		}
		_, _ = fmt.Fprintf(w, "      stdin stream  ·  %s%s\n", size, pwChip)
	}
}

// saveTargetLabel renders the receive destination for the accept prompt.
// Resolves the absolute path through displayPath so $HOME collapses to
// "~"; the trailing slash signals "this is a directory, not a file."
func saveTargetLabel(outDir string) string {
	return displayPath(outDir) + "/"
}

// sanitizeForDisplay strips control characters, Unicode format and
// bidirectional-override characters from peer-supplied text and caps it at
// maxLen runes. Shared guard for any untrusted string we print: without
// the bidi filter a peer can spoof display order with U+202E "RIGHT-TO-
// LEFT OVERRIDE" and friends — the textbook display-spoofing trick.
func sanitizeForDisplay(s string, maxLen int) string {
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
	return string(out)
}

// sanitizeRemote sanitizes a peer-supplied hostname for display, then
// strips the mDNS ".local" suffix that macOS / many Linuxes tack onto
// Bonjour hostnames, falling back to a neutral placeholder when empty.
func sanitizeRemote(s string) string {
	clean := strings.TrimSuffix(sanitizeForDisplay(s, 64), ".local")
	if clean == "" {
		// "peer" reads as a neutral placeholder instead of "(unknown)",
		// which can scan as "fsend failed to detect something". The
		// transfer is fine; we just don't have a hostname to display.
		return "peer"
	}
	return clean
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
