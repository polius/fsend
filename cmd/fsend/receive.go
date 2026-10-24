package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
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

// lanQueryWindow bounds how long the receiver asks for a same-LAN sender;
// lanHeadStart is how long it waits on that alone before also trying the
// pairing server. An mDNS answer that arrives after the server path
// started still wins until the join succeeds, so an offline LAN transfer
// survives a slow first answer (Wi-Fi power save).
const (
	lanQueryWindow = 1500 * time.Millisecond
	lanHeadStart   = 300 * time.Millisecond
)

// lanFirstDialTimeout bounds the QUIC connect to an mDNS-discovered
// sender. A failure means mDNS responded but the listener is unreachable
// — a stale announce, or a firewall silently dropping the port (Windows
// Defender blocks the listener by default while its built-in 5353 rule
// lets discovery work) — so we fall through to the server. RTT-scale: a
// real same-LAN connect takes milliseconds. The PAKE that follows gets
// the full HandshakeTimeout (see quicconn.DialTimeout).
const lanFirstDialTimeout = 2 * time.Second

// maxRepairRounds caps how often a receiver re-discovers the sender after
// its in-place retries run out (network change, laptop sleep).
const maxRepairRounds = 2

// peerCancelGrace is how long a failed attempt waits for the sender's
// close to say whether it was a deliberate cancel.
const peerCancelGrace = time.Second

// recvSession is the receiver's pairing state across rounds.
type recvSession struct {
	code    string
	key     pairKey
	resumed bool // key is a resume key: switched this run, or saved by an earlier one
}

func newRecvSession(c string) *recvSession {
	if k, ok := loadResumeKey(c); ok {
		return &recvSession{code: c, key: k, resumed: true}
	}
	return &recvSession{code: c, key: codeKey(c)}
}

// paired switches to the resume key on the first successful PAKE and
// saves it, so a rerun of this same command can still find the sender.
func (rs *recvSession) paired(binding []byte) {
	if rs.resumed {
		return
	}
	rs.key, rs.resumed = resumeKey(binding), true
	saveResumeKey(rs.code, rs.key)
}

// recvLink is one route to the sender: dial opens a fresh authenticated
// connection on it, close releases its sockets.
type recvLink struct {
	path  connpath.Info
	first *quicconn.AcceptResult // already-open first connection, if any
	dial  func(ctx context.Context, secret string) (*quicconn.AcceptResult, error)
	close func()
	relay *relayProbe // non-nil on the relay path
	lan   bool        // found by mDNS
}

// runReceive executes the receive-side flow: find the sender (same-LAN
// mDNS, else the pairing server with ICE/relay), then transfer with
// in-place retries. If those run out after a network change or sleep, it
// finds the sender again on fresh sockets — the sender re-pairs on the
// same resume key.
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
	// Sink mode writes no files, so --overwrite/--rename can only be a mistake.
	if f.outDir == "-" && (f.overwrite || f.rename) {
		return fmt.Errorf("%w: --overwrite and --rename have no effect with --out -", fserrors.ErrUsage)
	}
	if f.overwrite && f.rename {
		return fmt.Errorf("%w: --overwrite and --rename are mutually exclusive (replace, or keep both)", fserrors.ErrUsage)
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
	outDir, sink, err := resolveOutDir(f)
	if err != nil {
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
	startUpdateCheck(f)

	rs := newRecvSession(c)

	// Name the phase rather than showing a bare "Connecting": the LAN probe
	// and the server fallback are very different waits, and a named phase
	// turns the mystery pause into a status. One continuous spinner spans
	// both — its message changes in place, so a fast LAN hit never flashes.
	var spin *uxlog.Spinner
	if !f.quiet {
		spin = uxlog.StartSpinner("Looking for sender on the local network", uxlog.HintCancel)
	}
	var ui *receiverUI
	defer func() {
		spin.Stop()
		if ui != nil {
			ui.close()
		}
	}()

	start := time.Now()
	var dropErr error
	for round := 0; ; round++ {
		link, err := discoverSender(ctx, f, rs, spin)
		if err != nil {
			// A failed re-discovery is still the transfer drop the user saw.
			if dropErr != nil && ctx.Err() == nil {
				return dropErr
			}
			return err
		}
		if ui == nil {
			ui = newReceiverUI(ctx, f, outDir, sink, link.path)
		}
		ui.pathInfo = link.path
		// The pre-prompt phase is classification: stat-only and instant on
		// LAN unless --checksum hashes what's already on disk. Keep the
		// spinner for the slower cases; the prompt (or ui.close) stops it.
		if !link.lan || f.checksum {
			spin.SetMessage("Checking existing files")
			ui.spin = spin
		} else {
			spin.Stop()
		}

		err = receiveOver(ctx, f, rs, ui, sink, link)
		link.close()
		if err == nil {
			forgetResumeKey(c)
			return finishReceive(f, ui, time.Since(start))
		}
		// A Ctrl-C at an interactive prompt cancels ctx but surfaces as a
		// decline/target-exists error from the engine; report it as a
		// cancellation (E026) rather than that incidental error.
		if ctx.Err() != nil {
			printCancelKeptHint(f, ui)
			return ctx.Err()
		}
		if sink || round >= maxRepairRounds || !rs.resumed || !errors.Is(err, fserrors.ErrTransientFailure) {
			return err
		}
		dropErr = err
		spin.Stop()
		if !f.quiet {
			uxlog.Println(fmt.Sprintf("  %s Connection lost — looking for the sender again", uxlog.Retry()))
			spin = uxlog.StartSpinner("Reconnecting to the sender", uxlog.HintCancel)
		}
	}
}

// discoverSender finds the sender for rs's current key: a same-LAN mDNS
// answer, else the pairing server. The mDNS query gets a head start and
// keeps running while the server join is tried; whichever lands first
// wins.
func discoverSender(ctx context.Context, f *flags, rs *recvSession, spin *uxlog.Spinner) (*recvLink, error) {
	lanCtx, cancelLAN := context.WithCancel(ctx)
	defer cancelLAN()
	lanCh := make(chan lanResult, 1)
	go func() {
		q, err := landisc.Query(lanCtx, rs.key.rendezvous, lanQueryWindow)
		lanCh <- lanResult{q, err}
	}()

	head := time.NewTimer(lanHeadStart)
	defer head.Stop()
	race := (<-chan lanResult)(lanCh)
	select {
	case r := <-lanCh:
		race = nil
		if r.err == nil {
			if link := dialLAN(ctx, f, rs, r.q, spin); link != nil {
				return link, nil
			}
		}
	case <-head.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	if !f.quiet {
		spin.SetMessage("Connecting via the server")
	}
	client, _, err := signalingClient(loadConfig(f.quiet))
	if err != nil {
		return nil, err
	}
	opts := joinOpts{budget: joinRetryBudget, lan: race}
	if rs.resumed {
		opts.budget, opts.retryClaimed = resumeJoinBudget, true
	}
	for {
		joined, hit, err := joinRace(ctx, client, rs.key.rendezvous, f, spin, opts)
		if hit != nil {
			if link := dialLAN(ctx, f, rs, hit, spin); link != nil {
				return link, nil
			}
			opts.lan = nil
			continue
		}
		if err != nil {
			return nil, err
		}
		cancelLAN()
		return linkViaInternet(ctx, f, client, joined)
	}
}

// dialLAN connects to an mDNS-discovered sender, or returns nil to fall
// back to the server path (see lanFirstDialTimeout).
func dialLAN(ctx context.Context, f *flags, rs *recvSession, q *landisc.QueryResult, spin *uxlog.Spinner) *recvLink {
	// JoinHostPort so an IPv6 announce dials as "[addr]:port".
	addr := net.JoinHostPort(q.IP.String(), strconv.Itoa(q.Port))
	if f.debug && !f.quiet {
		fmt.Fprintln(os.Stderr, "    sender address:", addr)
	}
	if !f.quiet {
		spin.SetMessage("Connecting to the sender")
	}
	first, err := quicconn.DialTimeout(ctx, addr, rs.key.secret, lanFirstDialTimeout)
	if err != nil {
		// Debug-only: the transfer may still end up "Direct on local
		// network" via ICE, so this notice by default reads as the tool
		// contradicting itself.
		if f.debug && !f.quiet {
			fmt.Fprintln(os.Stderr, uxlog.Info(), "Local sender unreachable — falling back to server:", err)
		}
		return nil
	}
	return &recvLink{
		path:  connpath.FromLAN(),
		lan:   true,
		first: first,
		dial: func(ctx context.Context, secret string) (*quicconn.AcceptResult, error) {
			return quicconn.Dial(ctx, addr, secret)
		},
		close: func() {},
	}
}

// receiveOver runs the transfer on link with in-place retries: a
// mid-stream drop re-dials the same route and resumes from the
// receiver's partial. Sink mode gets one attempt: emitted bytes can't be
// reconciled, so a retry would duplicate output.
func receiveOver(ctx context.Context, f *flags, rs *recvSession, ui *receiverUI, sink bool, link *recvLink) error {
	opts := retry.Options{OnRetry: ui.retryNotice()}
	if sink {
		opts.Attempts = 1
	}
	err := retry.WithBackoff(ctx, opts, nil, func(attempt int) error {
		if attempt > 1 {
			ui.resetAttemptCounts()
		}
		res := link.first
		link.first = nil
		if res == nil {
			var err error
			if res, err = link.dial(ctx, rs.key.secret); err != nil {
				return fmt.Errorf("dialing sender: %w", err)
			}
		}
		defer res.Close()
		rs.paired(res.Streams.Binding)
		return recvOnce(ctx, f, ui, res)
	})
	if err != nil && link.relay != nil && ctx.Err() == nil {
		err = classifyRelayDrop(ctx, link.relay.client, link.relay.sessionID, link.relay.roleToken, err)
	}
	return err
}

// recvOnce runs the receive engine on one authenticated connection.
//
// The attempt is bound to the connection: a sender that goes away while
// the user sits at a prompt ends the prompt instead of hanging it. A
// sender that closed with the cancel code is reported as E032 at once.
func recvOnce(ctx context.Context, f *flags, ui *receiverUI, res *quicconn.AcceptResult) error {
	connCtx, stop := context.WithCancel(ctx)
	defer stop()
	defer context.AfterFunc(res.Conn.Context(), stop)()
	ui.ctx = connCtx
	defer func() { ui.ctx = ctx }()

	opts := ui.recvOptions(hostnameOrDefault(f.hostname))
	// A "no" that comes back after the connection died is the prompt being
	// aborted, not the user's answer.
	var promptAborted atomic.Bool
	accept := opts.Accept
	opts.Accept = func(h wire.SenderHello, s transfer.ClassifySummary) bool {
		ok := accept(h, s)
		if !ok && connCtx.Err() != nil {
			promptAborted.Store(true)
		}
		return ok
	}
	err := transfer.Recv(connCtx, &res.Streams, opts)
	if err == nil || ctx.Err() != nil {
		return err
	}
	connLost := promptAborted.Load() || (connCtx.Err() != nil && errors.Is(err, context.Canceled))
	// Only a drop-shaped error can hide a cancel; terminal answers (a
	// decline, a hash mismatch) need no wait.
	if quicconn.IsCancelClose(err) || (connLost || retry.IsTransient(err)) && res.PeerCancelled(peerCancelGrace) {
		return fserrors.ErrPeerCancelled
	}
	if connLost {
		// The connection died under us (a decline or cancel here is the
		// aborted prompt, not the user's answer): retry it like any drop.
		return fmt.Errorf("%w: connection lost: %v", fserrors.ErrConnectFailed, context.Cause(res.Conn.Context()))
	}
	return err
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
				fmt.Sprintf("  %s (%d/%d): ", uxlog.Prompt("Wrong password — try again"), attempt, transfer.PasswordAttempts), f.quiet)
		}
		// One line of context. The password is protocol-required before the
		// peer's HELLO can be decrypted, so the ordering is forced — but the
		// prompt can still say why it is asking.
		fmt.Fprintf(os.Stderr, "  %s This transfer is password-protected.\n", uxlog.Warn())
		return readPasswordHiddenCtx(ctx, "  "+uxlog.Prompt("Enter the password the sender shared: "), f.quiet)
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
			_, _ = fmt.Fprintf(w, "      stdin stream  ·  saves as %s  ·  size unknown%s\n", uxlog.Path(name), pwChip)
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
		// The wrapping folder/file name is a path — green, opencode-style.
		lead = uxlog.Path(name) + "  ·  " + fileCount
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
	return sanitizeRunes(s, maxLen, false)
}

// sanitizeTextPayload is the TTY-mode filter for peer-supplied --text:
// same strip as sanitizeForDisplay but keeps \n and \t so multi-line
// text stays readable. Piped stdout bypasses it — bytes must stay exact.
func sanitizeTextPayload(s string) string {
	return sanitizeRunes(s, math.MaxInt, true)
}

func sanitizeRunes(s string, maxLen int, keepWhitespace bool) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r < 0x20 {
			// \n and \t are the only control bytes a terminal renders
			// benignly; the rest (ESC, \r, BEL) are injection vectors.
			if keepWhitespace && (r == '\n' || r == '\t') {
				out = append(out, r)
			}
			continue
		}
		switch {
		case r == 0x7F, isBidi(r), unicode.Is(unicode.Cf, r):
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
