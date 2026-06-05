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
	"github.com/polius/fsend/internal/version"
	"github.com/polius/fsend/internal/wire"
)

// runReceive executes the receive-side flow.
//
// v0.1.0 LAN-only path:
//  1. Validate the code shape.
//  2. mDNS query for the sender (300ms timeout).
//  3. Dial QUIC at the discovered IP:port.
//  4. Run the transfer protocol with prompt callback.
func runReceive(f *flags, c string) error {
	if err := code.Validate(c); err != nil {
		return fserrors.ErrInvalidCodeFormat
	}

	ctx, cancel := signalContext()
	defer cancel()

	// LAN discovery first.
	if !f.quiet {
		fmt.Fprintln(os.Stderr, marker("⠋", "[*]"), "Looking for sender on local network…")
	}
	q, err := landisc.Query(ctx, c, 300*time.Millisecond)
	if err != nil {
		// LAN miss → try the rendezvous + relay path.
		if !f.quiet {
			fmt.Fprintln(os.Stderr, marker("⠋", "[*]"), "Not on LAN — connecting via rendezvous server…")
		}
		cfg, _ := config.Load()
		return runReceiveOverInternet(ctx, f, c, cfg)
	}

	addr := q.IP.String() + ":" + strconv.Itoa(q.Port)
	if !f.quiet {
		fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Found sender at", addr)
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

	closeProg, accept, progressFn := newReceiverProgress(f)
	defer closeProg()

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

	if !f.quiet {
		fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	}
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
// The input is echoed in v1: we deliberately avoid pulling in golang.org/x/term
// for the minor UX win of no-echo on TTYs. The expectation is that
// non-interactive flows use --pass or FSEND_PASS; the prompt is a fallback.
func receiverPasswordPrompt(f *flags) func() (string, error) {
	if f.quiet {
		return nil
	}
	return func() (string, error) {
		fmt.Fprintln(os.Stderr)
		fmt.Fprint(os.Stderr, "  Password required by sender: ")
		var pw string
		if _, err := fmt.Fscanln(os.Stdin, &pw); err != nil {
			return "", err
		}
		return pw, nil
	}
}

func promptAccept(f *flags, h wire.SenderHello) bool {
	// --quiet: no prompt block at all. --yes is required (we don't
	// interactively prompt without UX).
	if f.quiet {
		return f.yes
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  Receiving from", sanitizeRemote(h.Hostname))
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  ─────────────────────────────────────────────")
	fmt.Fprintln(os.Stderr)
	switch h.TransferKind {
	case wire.TransferSingleFile:
		fmt.Fprintf(os.Stderr, "      (%s)\n", humanBytes(int64(h.TotalBytes)))
	case wire.TransferDirectory:
		fmt.Fprintf(os.Stderr, "      directory  (%d files, %s)\n", h.TotalFiles, humanBytes(int64(h.TotalBytes)))
	case wire.TransferMultiFile:
		fmt.Fprintf(os.Stderr, "      %d items  (%s)\n", h.TotalFiles, humanBytes(int64(h.TotalBytes)))
	case wire.TransferText:
		fmt.Fprintln(os.Stderr, "      a piece of text")
	case wire.TransferStdin:
		fmt.Fprintln(os.Stderr, "      stream from stdin")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  ─────────────────────────────────────────────")
	fmt.Fprintln(os.Stderr)

	if f.yes {
		fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Accepting (--yes)")
		return true
	}
	fmt.Fprint(os.Stderr, "  Save to current directory? [Y/n]: ")
	var resp string
	_, _ = fmt.Fscanln(os.Stdin, &resp)
	switch resp {
	case "n", "N", "no":
		return false
	default:
		return true
	}
}

// sanitizeRemote removes control characters and ANSI sequences from peer-
// supplied strings before display (per docs/security/threat-model.md T7).
func sanitizeRemote(s string) string {
	const maxLen = 64
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7F {
			continue
		}
		// Reject ANSI Esc.
		if r == 0x1B {
			continue
		}
		out = append(out, r)
		if len(out) >= maxLen {
			break
		}
	}
	if len(out) == 0 {
		return "(unknown)"
	}
	return string(out)
}

