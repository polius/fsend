package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
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
	// --manifest records files written to disk; there are none with --out -.
	if f.outDir == "-" && f.manifest != "" {
		return fmt.Errorf("%w: --manifest has no effect with --out -", fserrors.ErrUsage)
	}
	// Sink mode gives stdout to the file bytes; NDJSON would corrupt them.
	if f.outDir == "-" && f.json {
		return fmt.Errorf("%w: --json cannot be combined with --out - (stdout carries the received bytes)", fserrors.ErrUsage)
	}
	// Validate --out before any network work: a missing directory would
	// otherwise only fail at write time, after the sender's one-shot code
	// has been consumed.
	if _, _, err := resolveOutDir(f); err != nil {
		return err
	}

	// The signal handler must be installed before any interactive prompt:
	// resolvePassword reads with echo off, and a default-disposition
	// SIGINT inside that read would kill the process without restoring
	// the terminal.
	ctx, cancel := signalContext(f.quiet)
	defer cancel()

	// Bare --password: hidden no-echo prompt. We're not the password's
	// author — we have to type what the sender configured — so there's
	// no point offering a random default here.
	if err := resolvePassword(ctx, f, false); err != nil {
		return err
	}

	// LAN discovery is a 300 ms probe — short enough that showing a
	// spinner only flashes a line that immediately gets cleared on miss,
	// which reads as glitchy. Stay silent through the probe; only show a
	// spinner once we know we're going down the longer internet path.
	q, err := landisc.Query(ctx, c, 300*time.Millisecond)
	if err != nil {
		// LAN miss → internet path. A single "Connecting" spinner runs
		// from here through Join + ICE/relay setup and classification,
		// replacing what used to be a sequence of brief flashes.
		// runReceiveOverInternet owns its lifetime.
		cfg := loadConfig(f.quiet)
		var spin *uxlog.Spinner
		if !f.quiet {
			spin = uxlog.StartSpinner("Connecting")
		}
		return runReceiveOverInternet(ctx, f, c, cfg, spin)
	}

	// JoinHostPort so an IPv6 announce dials as "[addr]:port".
	addr := net.JoinHostPort(q.IP.String(), strconv.Itoa(q.Port))
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
	// --checksum hashes the files already on disk before the accept prompt —
	// a silence that scales with the existing data. Cover it with a spinner
	// (stopped by the prompt). Without --checksum classification is stat-only
	// and near-instant; a spinner would just flash.
	if f.checksum && !f.quiet {
		ui.spin = uxlog.StartSpinner("Checking existing files")
	}
	defer ui.close()

	start := time.Now()

	// LAN succeeded once — keep a normal retry loop around the transfer
	// itself so a mid-stream drop can re-dial and resume from the
	// receiver's partial. Sink mode gets one attempt: emitted bytes
	// can't be reconciled, so a retry would duplicate output.
	opts := retry.Options{OnRetry: ui.retryNotice()}
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
			printCancelKeptHint(f, ui)
			return ctx.Err()
		}
		return err
	}

	return finishReceive(f, ui, time.Since(start))
}

// receiverPasswordPrompt returns a callback that reads a password from
// stdin when the sender requires --password but the receiver didn't supply
// one via --password / FSEND_PASSWORD. Returns nil under --quiet so the transfer
// engine immediately fails with ErrWrongPassword instead of blocking on
// a prompt that nobody will see.
//
// Input is read with no echo when stdin is a TTY (golang.org/x/term).
// Non-interactive callers should pass --password or FSEND_PASSWORD so the
// prompt never fires.
//
// Timing: the progress bar is constructed lazily on first byte (see
// newReceiverProgress), so this prompt fires before any bar exists —
// no risk of mpb rendering on top of the password input line.
func receiverPasswordPrompt(ctx context.Context, f *flags) func(attempt int) (string, error) {
	if f.quiet {
		return nil
	}
	return func(attempt int) (string, error) {
		fmt.Fprintln(os.Stderr)
		if attempt > 1 {
			return readPasswordHiddenCtx(ctx,
				fmt.Sprintf("  Wrong password — try again (%d/%d): ", attempt, transfer.PasswordAttempts), f.quiet)
		}
		return readPasswordHiddenCtx(ctx, "  Password for this transfer: ", f.quiet)
	}
}

// renderArtifact prints the indented artifact line and the classification
// breakdown. The breakdown is how the user learns, before consenting, that
// most files are already up to date and only a few will move.
func renderArtifact(w io.Writer, h wire.SenderHello, summary transfer.ClassifySummary, sink bool) {
	pwChip := ""
	if h.HasPassword {
		pwChip = "  " + uxlog.PasswordChip()
	}
	if h.Mode == wire.ModeStream {
		switch {
		case h.IsText:
			_, _ = fmt.Fprintf(w, "      text%s\n", pwChip)
		case sink:
			_, _ = fmt.Fprintf(w, "      stdin stream  ·  size unknown%s\n", pwChip)
		default:
			// The name is peer-supplied; show exactly what will land on disk
			// (same derivation as the engine) so consent covers the filename.
			name := sanitizeForDisplay(transfer.StreamFileName(h.DisplayName), 128)
			_, _ = fmt.Fprintf(w, "      stdin stream  ·  saves as %s  ·  size unknown%s\n", name, pwChip)
		}
		return
	}
	// DisplayName is peer-supplied; sanitize so a crafted name can't inject
	// ANSI/bidi into the prompt.
	name := sanitizeForDisplay(h.DisplayName, 128)
	if name == "" {
		name = "files"
	}
	// File count (not summary.Total) so it matches the sender and the rows
	// below — Total includes directories, which neither shows. For a contents
	// or multi-path send the peer's display name *is* the count (e.g.
	// "2 files"), so don't print it twice.
	fileCount := uxlog.CountNoun(len(summary.Files), "file")
	lead := fileCount
	if name != fileCount {
		lead = name + "  ·  " + fileCount
	}
	diff := ""
	if summary.Differing > 0 {
		diff = fmt.Sprintf("  ·  %d %s", summary.Differing, differVerb(summary.Differing))
	}
	_, _ = fmt.Fprintf(w, "      %s  ·  %s%s%s\n",
		lead, receiverSizeClause(summary), diff, pwChip)
	renderPreview(w, receiverPreview(summary.Files), 8)
}

// differVerb conjugates the "N differ(s)" clauses used by the incoming
// header and the overwrite prompt.
func differVerb(n int) string {
	if n == 1 {
		return "differs"
	}
	return "differ"
}

// receiverSizeClause renders the headline's size figure. The offered total
// always agrees with the sender (both derive it from the same listing); when
// the receiver will skip part of it, the clause reads "X of Y" so it also
// agrees with the progress bar, which fills to X. When nothing transfers and
// nothing conflicts it says so outright rather than printing a bare "0 B".
func receiverSizeClause(s transfer.ClassifySummary) string {
	offered := uxlog.HumanBytes(int64(s.OfferedBytes))
	if s.BytesToRecv == 0 {
		// Nothing auto-downloads. Distinguish "you already have it all" from
		// "everything here conflicts" (the latter keeps the offered size, and
		// the · N differ clause explains it).
		if s.Differing == 0 && s.Identical > 0 {
			return "already up to date"
		}
		return offered
	}
	net := uxlog.HumanBytes(int64(s.BytesToRecv))
	if net == offered {
		return offered // any skipping is negligible at display resolution
	}
	return net + " of " + offered
}

// receiverPreview projects the classified files into preview rows. Names and
// symlink targets are peer-supplied, so each is sanitized like every other
// untrusted string we print. The status annotates only the consent-relevant
// or partial rows; fresh files — the common case — stay unmarked.
func receiverPreview(files []transfer.SummaryEntry) []previewItem {
	items := make([]previewItem, 0, len(files))
	for _, f := range files {
		note := ""
		switch f.Status {
		case "identical":
			note = "up to date"
		case "differs":
			note = "differs"
		case "resume":
			note = "resume"
		}
		it := previewItem{
			name: sanitizeForDisplay(f.RelativePath, 128),
			size: f.Size,
			note: note,
		}
		if f.Type == wire.EntrySymlink {
			it.link = sanitizeForDisplay(f.SymlinkTarget, 128)
		}
		items = append(items, it)
	}
	return items
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
	}
	if len(out) <= maxLen {
		return string(out)
	}
	// Truncate in the middle with a visible ellipsis: this name is what
	// the user consents to, so a cut must not masquerade as complete, and
	// keeping the tail leaves the real extension in view.
	const tail = 16
	return string(out[:maxLen-tail-1]) + "…" + string(out[len(out)-tail:])
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
