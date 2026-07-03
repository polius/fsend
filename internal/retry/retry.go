// Package retry provides a tiny exponential-backoff helper used by the
// transfer orchestration layer to recover from transient network errors
// without surfacing them to the user.
//
// Scope is deliberately narrow: a `WithBackoff` function plus a
// well-tested classifier for what counts as transient. It adds equal
// jitter to each sleep so a fleet of clients recovering from the same
// outage don't retry in lockstep against a shared relay; anything beyond
// that (circuit breakers, persistence) belongs in a dedicated lib.
package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
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
	for attempt := 1; attempt <= opts.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := op(attempt)
		if err == nil {
			return nil
		}
		// Non-transient: caller is the right place to handle (e.g. wrong
		// password, hash mismatch). Bubble out unchanged.
		if !isTransient(err) {
			return err
		}
		// Transient but no budget left: wrap in ErrTransientFailure so the
		// catalog maps it to E020 ("Transfer was interrupted and retries
		// did not recover") instead of falling through to E099. The raw
		// underlying error (e.g. "QUIC accept: ...context deadline
		// exceeded") rarely matches any user-facing sentinel on its own.
		if attempt == opts.Attempts {
			return fmt.Errorf("%w: %v", fserrors.ErrTransientFailure, err)
		}
		sleep := jitter(wait)
		if opts.OnRetry != nil {
			opts.OnRetry(attempt+1, sleep, err)
		}
		// Sleep, but honor ctx cancellation.
		t := time.NewTimer(sleep)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		}
		// Grow the *nominal* schedule (1s, 3s, 9s…); jitter is re-applied
		// to each sleep above, not compounded into the next interval.
		wait *= 3
		if wait > opts.Max {
			wait = opts.Max
		}
	}
	// Unreachable: the loop always returns inside the body.
	return nil
}

// jitter applies equal jitter to a backoff interval: keep half, randomize
// the other half, giving a sleep in [d/2, d]. Spreads a fleet's retries so
// they don't all reconnect at the same instant after a shared outage.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	half := d / 2
	return half + time.Duration(rand.Int64N(int64(half)+1))
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
	//
	// "canceled by remote" is quic-go's StreamError: the peer's teardown
	// cancels its data-stream read (STOP_SENDING) just before the
	// connection close, and an in-flight write can surface the stream
	// cancel instead of the "Application error" above — same abrupt
	// close, same transient verdict.
	//
	// "failed to write STUN message to ICE connection" is pion/ice's
	// sentinel from its STUN/data demux guard: ice.Conn.Write refuses any
	// app packet that stun.IsMessage flags (len>=20 && bytes[4:8]==magic
	// cookie). A QUIC datagram whose encrypted bytes 4..7 randomly equal
	// the cookie trips it — a false positive, never a real network fault.
	// A retry re-dials with fresh connection IDs/keys so the colliding
	// bytes can't recur (and the STUN-safe connID generator prevents it
	// outright); always safe to retry. See internal/quicconn/connid.go.
	if s := err.Error(); strings.Contains(s, "idle timeout") ||
		strings.Contains(s, "connection reset by peer") ||
		strings.Contains(s, "use of closed network connection") ||
		strings.Contains(s, "Application error 0x") ||
		strings.Contains(s, "canceled by remote") ||
		strings.Contains(s, "failed to write STUN message to ICE connection") {
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
	fserrors.ErrPasswordRequired,   // receiver has no password to offer; retrying won't conjure one
	fserrors.ErrCodeAlreadyClaimed, // sender already paired; no retry will recover
	fserrors.ErrWriteFailed,        // peer's (or our) disk problem; retrying won't fix it
	fserrors.ErrReadFailed,         // peer's (or our) source unreadable; retrying won't fix it
	fserrors.ErrDiskFull,
	fserrors.ErrPeerCancelled,       // peer hit Ctrl-C; it isn't coming back
	fserrors.ErrIncompatibleVersion, // version mismatch; only an update fixes it
	context.Canceled,                // user hit Ctrl-C
}
