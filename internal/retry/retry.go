// Package retry provides a tiny exponential-backoff helper used by the
// transfer orchestration layer to recover from transient network errors
// without surfacing them to the user.
//
// Scope is deliberately narrow: a `WithBackoff` function plus a
// well-tested classifier for what counts as transient. Anything more
// (jitter, circuit breakers, persistence) belongs in a dedicated lib —
// fsend doesn't need it.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/polius/fsend/internal/fserrors"
)

// Options configures one WithBackoff invocation. Zero values pick the
// fsend defaults (3 attempts, 1 s base, 3× multiplier).
type Options struct {
	// Attempts caps the total number of tries (including the first).
	// 0 → use the default (3).
	Attempts int
	// Base is the first sleep interval; subsequent sleeps multiply by 3.
	// 0 → use the default (1s, giving 1s, 3s, 9s for a 3-attempt run).
	Base time.Duration
	// Max caps any single sleep interval. 0 → use the default (30s).
	Max time.Duration
	// OnRetry, if non-nil, is called between attempts. attempt is
	// 1-indexed for the *upcoming* attempt; the wait is how long
	// we're about to sleep before it. The hook is the right place to
	// print a "retrying…" line to stderr.
	OnRetry func(attempt int, wait time.Duration, lastErr error)
}

// DefaultAttempts is the attempt cap used when Options.Attempts is zero.
// Exported so callers can display "(attempt n/N)" without re-deriving it.
const DefaultAttempts = 3

// defaultOptions returns the values used when fields are zero.
func defaultOptions() Options {
	return Options{Attempts: DefaultAttempts, Base: time.Second, Max: 30 * time.Second}
}

// WithBackoff runs op until it succeeds, returns a non-transient error,
// or the attempt budget is exhausted.
//
// op receives the 1-indexed attempt number so it can log its own
// per-attempt context (e.g. "ICE attempt 2/3"). The classifier
// IsTransient is used to distinguish retryable errors from terminal
// ones; pass nil to use the default classifier.
func WithBackoff(ctx context.Context, opts Options, isTransient func(error) bool, op func(attempt int) error) error {
	if opts.Attempts == 0 {
		opts.Attempts = defaultOptions().Attempts
	}
	if opts.Base == 0 {
		opts.Base = defaultOptions().Base
	}
	if opts.Max == 0 {
		opts.Max = defaultOptions().Max
	}
	if isTransient == nil {
		isTransient = IsTransient
	}

	wait := opts.Base
	var lastErr error
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(attempt)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isTransient(err) || attempt == opts.Attempts {
			return err
		}
		if opts.OnRetry != nil {
			opts.OnRetry(attempt+1, wait, err)
		}
		// Sleep, but honor ctx cancellation.
		t := time.NewTimer(wait)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
		wait *= 3
		if wait > opts.Max {
			wait = opts.Max
		}
	}
	// Unreachable; loop always returns.
	return fmt.Errorf("%w: %v", fserrors.ErrTransientFailure, lastErr)
}

// IsTransient is the default classifier. An error is transient when
// it is one of:
//
//   - context.DeadlineExceeded (sub-budget timeouts during a step)
//   - io.EOF or io.ErrUnexpectedEOF (peer closed mid-frame)
//   - any net.Error whose Timeout() is true (socket-level timeouts)
//   - a fserrors.ErrConnectFailed wrap (the receiver's "stream closed
//     mid-file" surface)
//   - a QUIC IdleTimeoutError (peer went silent within idle window)
//
// Non-transient (returned as-is): hash mismatches, receiver-declined,
// protocol errors, path traversal, partial-mismatch. These represent
// fundamental disagreement between the peers; retrying won't help and
// could mask a real problem.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}

	// Hard "do not retry" cases first — short-circuit before the
	// generic Timeout checks below catch a wrapped version of one of
	// these by accident.
	for _, terminal := range terminalSentinels {
		if errors.Is(err, terminal) {
			return false
		}
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, fserrors.ErrConnectFailed) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	// QUIC's IdleTimeoutError doesn't satisfy net.Error in all cases;
	// match by error-string fallback. This is brittle but the
	// alternative — importing quic-go just for one type — pulls the
	// retry package into the transport's dependency graph.
	//
	// "Application error 0x..." is the shape quic-go renders when either
	// side closes the connection with CloseWithError. In fsend this fires
	// when a peer is killed mid-transfer (Ctrl-C, OOM, network drop) and
	// the surviving side's next read/write surfaces the abrupt close. The
	// peer can come back on a retry and resume from its .fsend-partial,
	// so this is the canonical transient case — without this match it
	// surfaced as E099 "please file an issue".
	//
	// Terminal disagreements (wrong password, hash mismatch, peer
	// declined, ...) are surfaced via wire-level ErrorFrames BEFORE the
	// QUIC stream tears down, so they're already caught by the terminal
	// sentinel check above and never reach this fallback.
	if s := err.Error(); strings.Contains(s, "idle timeout") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "Application error 0x") {
		return true
	}

	return false
}

// terminalSentinels are errors that must never be retried. Listed
// explicitly so a stray retry-wrapping doesn't turn a real user-visible
// failure into "retried 3 times, then <same error>" — same outcome,
// worse UX.
var terminalSentinels = []error{
	fserrors.ErrHashMismatch,
	fserrors.ErrReceiverDeclined,
	fserrors.ErrPartialMismatch,
	fserrors.ErrInvalidCodeFormat,
	fserrors.ErrPathTraversal,
	fserrors.ErrProtocolError,
	fserrors.ErrTargetExists,
	fserrors.ErrServerRetired,
	fserrors.ErrWrongPassword,      // wrong password is one-shot per session
	fserrors.ErrCodeAlreadyClaimed, // sender already paired; no retry will recover
	context.Canceled,               // user hit Ctrl-C
}
