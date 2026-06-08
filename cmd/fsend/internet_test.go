package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
)

// joinWithRetry must succeed when the sender Creates after the receiver
// has already started polling — this is the race the retry exists to
// paper over. Without it, the receiver fails instantly with E002 when
// it pastes the code a hair before the sender registers.
func TestJoinWithRetry_SucceedsWhenSenderArrivesLate(t *testing.T) {
	srv, baseURL := newSignalingTestServer(t)
	_ = srv

	senderClient := signaling.New(baseURL, "test")
	receiverClient := signaling.New(baseURL, "test")
	code := "abc-defg-jkm"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Receiver starts polling first; sender Creates after a short delay.
	// joinWithRetry should ride out the 404 window and succeed.
	var wg sync.WaitGroup
	var joinErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, joinErr = joinWithRetry(ctx, receiverClient, code, &flags{quiet: true}, nil)
	}()

	time.Sleep(500 * time.Millisecond) // ~2-3 retry cycles
	if _, err := senderClient.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}

	wg.Wait()
	if joinErr != nil {
		t.Errorf("joinWithRetry should have succeeded once sender Created, got %v", joinErr)
	}
}

// When the sender never arrives, joinWithRetry must surface the original
// ErrCodeNotFound after the budget — not get stuck and not mangle the
// error type (the catalog renderer relies on errors.Is for E002).
func TestJoinWithRetry_GivesUpAfterBudget(t *testing.T) {
	_, baseURL := newSignalingTestServer(t)
	client := signaling.New(baseURL, "test")

	// Shrink the budget for the test by overriding it.
	orig := joinRetryBudget
	defer func() { joinRetryBudget = orig }()
	joinRetryBudget = 500 * time.Millisecond

	start := time.Now()
	_, err := joinWithRetry(context.Background(), client, "abc-defg-jkm", &flags{quiet: true}, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, fserrors.ErrCodeNotFound) {
		t.Errorf("expected ErrCodeNotFound after budget, got %v", err)
	}
	if elapsed < 400*time.Millisecond {
		t.Errorf("gave up too early: %v (expected to retry through the full budget)", elapsed)
	}
}

// Errors other than ErrCodeNotFound must surface on the first try — we
// don't want to retry through 5s of, say, server-unreachable.
func TestJoinWithRetry_NonRetriableErrorPropagatesImmediately(t *testing.T) {
	// Bogus port → connection refused → ErrServerUnreachable.
	client := signaling.New("http://127.0.0.1:1", "test")
	start := time.Now()
	_, err := joinWithRetry(context.Background(), client, "abc-defg-jkm", &flags{quiet: true}, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, fserrors.ErrServerUnreachable) {
		t.Errorf("expected ErrServerUnreachable to propagate, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("retried a non-retriable error: %v elapsed", elapsed)
	}
}

// Each eviction reason the relay reports must map to its sentinel; any
// other state must leave runErr untouched.
func TestClassifyRelayDrop_MapsReasons(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		runErr    error
		wantErr   error
		wantSame  bool // when true, expect runErr to be returned unchanged
	}{
		{
			name:    "cap_hit_promotes_to_sentinel",
			body:    `{"state":"evicted","reason":"cap_hit"}`,
			runErr:  errors.New("idle timeout"),
			wantErr: fserrors.ErrRelayCapHit,
		},
		{
			name:    "idle_promotes_to_sentinel",
			body:    `{"state":"evicted","reason":"idle"}`,
			runErr:  errors.New("idle timeout"),
			wantErr: fserrors.ErrRelayIdleTimeout,
		},
		{
			name:     "active_keeps_run_err",
			body:     `{"state":"active"}`,
			runErr:   errors.New("idle timeout"),
			wantSame: true,
		},
		{
			name:     "unknown_keeps_run_err",
			body:     `{"state":"unknown"}`,
			runErr:   errors.New("idle timeout"),
			wantSame: true,
		},
		{
			name:     "unmapped_reason_keeps_run_err",
			body:     `{"state":"evicted","reason":"future_reason"}`,
			runErr:   errors.New("idle timeout"),
			wantSame: true,
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/relay/status" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			defer ts.Close()
			client := signaling.New(ts.URL, "test")
			got := classifyRelayDrop(context.Background(), client, "sess-1", c.runErr)
			if c.wantSame {
				if got != c.runErr {
					t.Errorf("expected runErr unchanged, got %v", got)
				}
				return
			}
			if !errors.Is(got, c.wantErr) {
				t.Errorf("got %v, want %v", got, c.wantErr)
			}
		})
	}
}

// A successful transfer (nil runErr) must not get a sentinel pinned on it.
func TestClassifyRelayDrop_NilStaysNil(t *testing.T) {
	client := signaling.New("http://127.0.0.1:1", "test") // unreachable; not called
	if err := classifyRelayDrop(context.Background(), client, "sess-1", nil); err != nil {
		t.Errorf("nil runErr should pass through, got %v", err)
	}
}

// A failing status probe must not mask the underlying runErr.
func TestClassifyRelayDrop_ProbeFailureFallsBackToRunErr(t *testing.T) {
	client := signaling.New("http://127.0.0.1:1", "test")
	runErr := errors.New("idle timeout")
	got := classifyRelayDrop(context.Background(), client, "sess-1", runErr)
	if got != runErr {
		t.Errorf("expected runErr to survive a probe failure, got %v", got)
	}
}

func newSignalingTestServer(t *testing.T) (*server.Server, string) {
	t.Helper()
	s := server.New(server.Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          5 * time.Second,
		PairedTTL:            5 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts.URL
}
