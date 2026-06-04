package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"time"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/quicconn"
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

	// LAN discovery first (per PROJECT_SPEC.md connection flow step 3).
	fmt.Fprintln(os.Stderr, marker("⠋", "[*]"), "Looking for sender on local network…")
	q, err := landisc.Query(ctx, c, 300*time.Millisecond)
	if err != nil {
		// LAN miss is expected in many cases. v0.1.0 has no internet fallback,
		// so this is a hard error for now.
		return fmt.Errorf("%w (no internet fallback in v0.1.0; both peers must be on the same LAN)", fserrors.ErrCodeNotFound)
	}

	addr := q.IP.String() + ":" + strconv.Itoa(q.Port)
	fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Found sender at", addr)

	res, err := quicconn.Dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("dialing sender: %w", err)
	}
	defer res.Close()

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

	accept := func(h wire.SenderHello) bool {
		return promptAccept(f, h)
	}

	fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Direct connection established (LAN)")

	if err := transfer.Recv(ctx, &res.Streams, transfer.RecvOptions{
		Hostname:      hostname,
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TargetDir:     outDir,
		Overwrite:     f.overwrite,
		Accept:        accept,
	}); err != nil {
		return err
	}

	fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	return nil
}

func promptAccept(f *flags, h wire.SenderHello) bool {
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

// errUnused silences any errors-import linter checks.
var _ = errors.New
