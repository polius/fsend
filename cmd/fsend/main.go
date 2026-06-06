// Command fsend is the user-facing CLI for peer-to-peer file transfers.
//
// Dispatch follows PROJECT_SPEC.md "Dispatch rules":
//   - fsend                  → help
//   - fsend <code>           → receive (when <code> matches the code regex
//                              and no file with that name is in CWD)
//   - fsend <path>           → send
//   - fsend -                → send from stdin
//   - fsend --send / --receive force mode (skip auto-detect)
//
// v0.1.0 supports LAN-only operation (mDNS discovery + QUIC transfer).
// Rendezvous, ICE, and relay layers wire in later under the same CLI
// surface — no user-visible API changes when they land.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/uxlog"
)

func main() {
	// Normalise --connect arguments before cobra parses. We use
	// NoOptDefVal so bare `fsend --connect` prints the current server,
	// but that turns the natural `fsend --connect default` into
	// "show + leftover positional". Re-glue the value with `=` here so
	// both spellings work as users expect:
	//
	//   fsend --connect                       → show
	//   fsend --connect default               → set to default
	//   fsend --connect host:port [password]  → set custom
	os.Args = normalizeConnectArgs(os.Args)

	if err := rootCmd().Execute(); err != nil {
		os.Exit(renderError(err, debugRequested()))
	}
}

// normalizeConnectArgs rewrites `--connect VALUE` to `--connect=VALUE` so
// the value rides with the flag instead of falling through as a
// positional. Subsequent positionals (e.g. an optional password) are
// concatenated with a comma — StringSliceVar handles that natively.
//
// The transformation is conservative: anything that looks like a flag
// boundary (next token starts with "-", or there's no next token) is
// left alone so `fsend --connect` keeps triggering the bare-flag "show
// current server" path via NoOptDefVal.
func normalizeConnectArgs(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		a := args[i]
		// Already in `--connect=...` form, or any non-target flag.
		if a != "--connect" {
			out = append(out, a)
			continue
		}
		// Bare `--connect` at end → keep as-is (show current server).
		if i+1 >= len(args) {
			out = append(out, a)
			continue
		}
		next := args[i+1]
		// Next token is another flag → bare form intended.
		if len(next) > 0 && next[0] == '-' {
			out = append(out, a)
			continue
		}
		// Consume the next positional as the value. If a second
		// positional follows that isn't a flag, glue it on too so
		// `--connect host:port password` works.
		joined := next
		i++
		if i+1 < len(args) {
			peek := args[i+1]
			if len(peek) > 0 && peek[0] != '-' {
				joined = joined + "," + peek
				i++
			}
		}
		out = append(out, a+"="+joined)
	}
	return out
}

// renderError prints the catalog rendering of err to stderr and returns
// the exit code the process should use. When debug is set, the wrap
// chain is appended after the user-facing message so bug reports
// include the underlying technical details.
func renderError(err error, debug bool) int {
	// Treat Ctrl-C / SIGTERM as a clean user cancel, not an "unexpected
	// error". The signal handler in signalContext() cancels ctx, which
	// propagates as context.Canceled through the call stack. Doing the
	// remap here means both the rendered message *and* the exit code
	// come from ErrUserCancelled's catalog entry.
	if errors.Is(err, context.Canceled) {
		err = fserrors.ErrUserCancelled
	}
	entry, known := fserrors.Lookup(err)

	// For usage / source-not-found errors the wrap context is the
	// useful part ("invalid --mode \"foo\"", "/path/to/missing.txt").
	// Splice it under the catalog Message so users see WHY without
	// having to enable --debug.
	detail := ""
	if known && (errors.Is(err, fserrors.ErrUsage) || errors.Is(err, fserrors.ErrSourceNotFound)) {
		if extra := extractDetail(err.Error(), entry.Message); extra != "" {
			detail = extra
		}
	}

	// Pick the leading glyph based on severity: warnings (Exit==0, e.g.
	// ErrConfigCorrupted) get ⚠, real failures get ✗. Without this,
	// every catalog entry — including "this is fine, falling back to
	// defaults" — would be flagged with a red cross.
	glyph := uxlog.Cross()
	if entry.Exit == 0 {
		glyph = uxlog.Warn()
	}

	if detail != "" {
		fmt.Fprintf(os.Stderr, "%s [%s] %s\n  %s\n", glyph, entry.Code, entry.Message, detail)
		if entry.Action != "" {
			fmt.Fprintf(os.Stderr, "  %s\n", entry.Action)
		}
	} else {
		fmt.Fprintf(os.Stderr, "%s %s\n", glyph, entry.Render())
	}

	if debug {
		for _, c := range fserrors.Chain(err) {
			fmt.Fprintf(os.Stderr, "  DEBUG: %s\n", c)
		}
	}
	return entry.Exit
}

// extractDetail pulls the wrapper context out of a wrapped sentinel's
// Error() string. For `fmt.Errorf("%w: <ctx>", sentinel)`, .Error() is
// "<sentinel-msg>: <ctx>" — we strip the leading sentinel text so the
// user sees just the contextual part.
func extractDetail(full, catalogMsg string) string {
	// Find the first ": " — that's the boundary the %w + %v wrapper
	// produces. Whatever comes after is the caller's context.
	for i := 0; i+1 < len(full); i++ {
		if full[i] == ':' && full[i+1] == ' ' {
			return full[i+2:]
		}
	}
	// No wrapper context; fall back to nothing extra. We deliberately
	// don't echo the catalog message back to the user.
	_ = catalogMsg
	return ""
}

// debugRequested reports whether the user asked for debug output via
// the FSEND_DEBUG env var. The --debug flag is read here too via os.Args
// scanning so we don't need to re-parse cobra state after an error.
func debugRequested() bool {
	if v := os.Getenv("FSEND_DEBUG"); v != "" && v != "0" && v != "false" {
		return true
	}
	for _, a := range os.Args[1:] {
		if a == "--debug" {
			return true
		}
		if a == "--" {
			break
		}
	}
	return false
}
