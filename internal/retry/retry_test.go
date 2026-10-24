package retry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/polius/fsend/internal/fserrors"
)

func TestWithBackoff_SucceedsOnFirstAttempt(t *testing.T) {
	calls := 0
	err := WithBackoff(context.Background(), Options{Attempts: 3, Base: time.Millisecond}, nil,
		func(attempt int) error {
			calls++
			return nil
		})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 call, got %d", calls)
	}
}

func TestWithBackoff_RetriesTransient(t *testing.T) {
	calls := 0
	err := WithBackoff(context.Background(), Options{Attempts: 3, Base: time.Millisecond}, nil,
		func(attempt int) error {
			calls++
			if calls < 3 {
				return io.ErrUnexpectedEOF
			}
			return nil
		})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("want 3 calls, got %d", calls)
	}
}

func TestWithBackoff_DoesNotRetryTerminal(t *testing.T) {
	calls := 0
	err := WithBackoff(context.Background(), Options{Attempts: 3, Base: time.Millisecond}, nil,
		func(attempt int) error {
			calls++
			return fserrors.ErrHashMismatch
		})
	if !errors.Is(err, fserrors.ErrHashMismatch) {
		t.Fatalf("want ErrHashMismatch, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("want 1 call (no retries on terminal), got %d", calls)
	}
}

func TestWithBackoff_ExhaustsBudget(t *testing.T) {
	calls := 0
	err := WithBackoff(context.Background(), Options{Attempts: 2, Base: time.Millisecond}, nil,
		func(attempt int) error {
			calls++
			return io.EOF
		})
	if err == nil {
		t.Fatalf("expected error after exhaustion")
	}
	// Exhausted-transient must wrap ErrTransientFailure so the catalog
	// maps it to E020. Without this wrap, the raw underlying error (often
	// not in the catalog) falls through to E099.
	if !errors.Is(err, fserrors.ErrTransientFailure) {
		t.Fatalf("expected ErrTransientFailure wrap, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("want 2 calls, got %d", calls)
	}
}

func TestWithBackoff_RespectsCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := WithBackoff(ctx, Options{Attempts: 5, Base: 50 * time.Millisecond}, nil,
		func(attempt int) error {
			calls++
			return io.ErrUnexpectedEOF
		})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// First call always runs; cancellation should hit during the first backoff
	// or before the second call.
	if calls < 1 || calls > 2 {
		t.Fatalf("expected 1–2 calls before cancel, got %d", calls)
	}
}

func TestWithBackoff_OnRetryCallback(t *testing.T) {
	var seen []int
	err := WithBackoff(context.Background(), Options{
		Attempts: 3, Base: time.Millisecond,
		OnRetry: func(attempt int, wait time.Duration, _ error) {
			seen = append(seen, attempt)
		},
	}, nil, func(attempt int) error {
		if attempt < 3 {
			return io.EOF
		}
		return nil
	})
	if err != nil {
		t.Fatalf("want nil, got %v", err)
	}
	want := []int{2, 3}
	if len(seen) != len(want) {
		t.Fatalf("OnRetry called with %v, want %v", seen, want)
	}
	for i, v := range want {
		if seen[i] != v {
			t.Fatalf("OnRetry[%d] = %d, want %d", i, seen[i], v)
		}
	}
}

func TestIsTransient_Classification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil_is_not_transient", nil, false},
		{"io_eof_transient", io.EOF, true},
		{"io_unexpected_eof_transient", io.ErrUnexpectedEOF, true},
		{"connect_failed_transient", fserrors.ErrConnectFailed, true},
		{"wrapped_connect_failed_transient",
			fmt.Errorf("recv: stream closed: %w", fserrors.ErrConnectFailed), true},
		{"deadline_exceeded_transient", context.DeadlineExceeded, true},
		{"idle_timeout_string_transient", errors.New("Application error 0x0: idle timeout"), true},

		// Peer-abort: receiver Ctrl-C'd mid-transfer and the sender's
		// next chunk write surfaces "Application error 0x0 (remote)".
		// Must retry — the peer can come back and resume from its
		// partial in .fsend/. Without this, E099 lies about a routine
		// interruption.
		{"quic_application_error_remote_transient",
			errors.New("wire: writing chunk payload: Application error 0x0 (remote)"), true},
		{"quic_application_error_local_transient",
			errors.New("Application error 0x0 (local)"), true},
		{"quic_application_error_with_message_transient",
			errors.New("Application error 0x100 (remote): peer left"), true},

		// The peer's teardown cancels its data-stream read (STOP_SENDING)
		// just before the connection close; an in-flight write can surface
		// the stream cancel instead of the application error above. Same
		// abrupt close, same verdict. This is the exact field string.
		{"quic_stream_cancel_remote_transient",
			errors.New("wire: writing chunk payload: stream 3 canceled by remote with error code 0"), true},
		{"quic_stream_cancel_local_not_transient",
			errors.New("stream 3 canceled by local with error code 0"), false},

		// The same CLASSES as typed quic-go errors: the primary match is by
		// type, so these must classify correctly even if quic-go rewords its
		// Error() strings.
		{"quic_idle_timeout_typed_transient", &quic.IdleTimeoutError{}, true},
		{"quic_application_error_typed_remote_transient",
			fmt.Errorf("wire: writing chunk payload: %w", &quic.ApplicationError{Remote: true}), true},
		{"quic_application_error_typed_local_transient",
			&quic.ApplicationError{Remote: false}, true},
		{"quic_stream_cancel_typed_remote_transient",
			fmt.Errorf("wire: writing chunk payload: %w", &quic.StreamError{StreamID: 3, Remote: true}), true},
		{"quic_stream_cancel_typed_local_not_transient",
			&quic.StreamError{StreamID: 3, Remote: false}, false},

		// pion/ice STUN-demux false positive: a QUIC datagram's encrypted
		// bytes 4..7 randomly matched the STUN magic cookie, so
		// ice.Conn.Write rejected it. Never a real network fault; a re-dial
		// with fresh connection IDs dodges it, so it must retry rather than
		// surface as E099. This is the exact field-report string.
		{"ice_stun_write_false_positive_transient",
			errors.New("wire: writing chunk payload: failed to write STUN message to ICE connection"), true},

		// Network changes (roam, sleep, VPN reconnect): recoverable on
		// fresh sockets, so they must retry rather than end the transfer.
		{"enetunreach_transient",
			&net.OpError{Op: "write", Net: "udp", Err: os.NewSyscallError("sendto", syscall.ENETUNREACH)}, true},
		{"eaddrnotavail_transient",
			fmt.Errorf("dial: %w", os.NewSyscallError("bind", syscall.EADDRNOTAVAIL)), true},
		{"ehostunreach_transient", syscall.EHOSTUNREACH, true},
		{"ice_no_candidate_pairs_transient",
			errors.New("wire: writing chunk payload: no candidate pairs available"), true},

		// The peer's deliberate cancel close is final.
		{"quic_cancel_close_remote_terminal",
			fmt.Errorf("recv: %w", &quic.ApplicationError{Remote: true, ErrorCode: 0x10}), false},

		// Terminal cases — explicitly tested because a wrapping mistake
		// here would silently turn user errors into 3-retry hangs.
		{"hash_mismatch_terminal", fserrors.ErrHashMismatch, false},
		{"receiver_declined_terminal", fserrors.ErrReceiverDeclined, false},
		{"partial_mismatch_terminal", fserrors.ErrPartialMismatch, false},
		{"protocol_error_terminal", fserrors.ErrProtocolError, false},
		{"code_already_claimed_terminal", fserrors.ErrCodeAlreadyClaimed, false},
		{"wrapped_code_already_claimed_terminal",
			fmt.Errorf("lan dial: %w", fserrors.ErrCodeAlreadyClaimed), false},
		{"ctx_canceled_terminal", context.Canceled, false},
		{"password_required_terminal", fserrors.ErrPasswordRequired, false},
		{"peer_cancelled_terminal", fserrors.ErrPeerCancelled, false},

		// Wrapped terminal: must still not retry.
		{"wrapped_hash_mismatch_terminal",
			fmt.Errorf("verify: %w", fserrors.ErrHashMismatch), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsTransient(c.err); got != c.want {
				t.Errorf("IsTransient(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestWithBackoff_NegativeAttemptsFails pins the latent API trap: negative
// Attempts used to skip the loop and report success without running op.
func TestWithBackoff_NegativeAttemptsFails(t *testing.T) {
	called := false
	err := WithBackoff(context.Background(), Options{Attempts: -1}, nil,
		func(int) error { called = true; return nil })
	if err == nil {
		t.Fatal("negative Attempts must return an error, got nil")
	}
	if called {
		t.Error("op must not run when Attempts is negative")
	}
}
