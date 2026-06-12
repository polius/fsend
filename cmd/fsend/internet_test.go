package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
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

// A mistyped code must report E002, not E017: every 404 join burns the
// server's per-IP budget, so a strict server can start throttling our
// own retry loop mid-budget. Once we've seen "not found", a rate-limit
// response means "still not found, stop asking" — surfacing it as "too
// many attempts from your network" blames the user for our retries.
func TestJoinWithRetry_RateLimitAfterNotFoundReportsNotFound(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()
	client := signaling.New(ts.URL, "test")

	orig := joinRetryBudget
	defer func() { joinRetryBudget = orig }()
	joinRetryBudget = 30 * time.Second // must exit on the 429, not the budget

	start := time.Now()
	_, err := joinWithRetry(context.Background(), client, "abc-defg-jkm", &flags{quiet: true}, nil)

	if !errors.Is(err, fserrors.ErrCodeNotFound) {
		t.Errorf("expected ErrCodeNotFound when rate-limited after 404s, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("expected to stop on the first 429 (3 calls), got %d", got)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("kept retrying after the 429: %v elapsed", elapsed)
	}
}

// A rate limit on the very first join is the genuine article (someone
// behind this NAT really is hammering the server) and must surface as-is.
func TestJoinWithRetry_ImmediateRateLimitPropagates(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer ts.Close()

	_, err := joinWithRetry(context.Background(), signaling.New(ts.URL, "test"), "abc-defg-jkm", &flags{quiet: true}, nil)
	if !errors.Is(err, fserrors.ErrRateLimited) {
		t.Errorf("expected ErrRateLimited on first-attempt 429, got %v", err)
	}
}

// Each eviction reason the relay reports must map to its sentinel; any
// other state must leave runErr untouched. When the server includes a
// limit value, it must surface as a wrap detail so the renderer can
// print it on its own line; without a value, the bare sentinel must
// pass through cleanly so old servers don't break the user message.
func TestClassifyRelayDrop_MapsReasons(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		runErr     error
		wantErr    error
		wantDetail string // substring expected in err.Error() (skipped when empty)
		wantSame   bool   // when true, expect runErr to be returned unchanged
	}{
		{
			name:       "cap_hit_with_limit_includes_value",
			body:       `{"state":"evicted","reason":"cap_hit","limit_bytes":100000000}`,
			runErr:     errors.New("idle timeout"),
			wantErr:    fserrors.ErrRelayCapHit,
			wantDetail: "Server limit: 100 MB",
		},
		{
			name:    "cap_hit_without_limit_is_bare_sentinel",
			body:    `{"state":"evicted","reason":"cap_hit"}`,
			runErr:  errors.New("idle timeout"),
			wantErr: fserrors.ErrRelayCapHit,
		},
		{
			name:    "idle_is_bare_sentinel",
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
			got := classifyRelayDrop(context.Background(), client, "sess-1", "tok-1", c.runErr)
			if c.wantSame {
				if got != c.runErr {
					t.Errorf("expected runErr unchanged, got %v", got)
				}
				return
			}
			if !errors.Is(got, c.wantErr) {
				t.Errorf("got %v, want %v", got, c.wantErr)
			}
			if c.wantDetail != "" && !strings.Contains(got.Error(), c.wantDetail) {
				t.Errorf("got %q, want detail containing %q", got.Error(), c.wantDetail)
			}
		})
	}
}

// A successful transfer (nil runErr) must not get a sentinel pinned on it.
func TestClassifyRelayDrop_NilStaysNil(t *testing.T) {
	client := signaling.New("http://127.0.0.1:1", "test") // unreachable; not called
	if err := classifyRelayDrop(context.Background(), client, "sess-1", "tok-1", nil); err != nil {
		t.Errorf("nil runErr should pass through, got %v", err)
	}
}

// A failing status probe must not mask the underlying runErr.
func TestClassifyRelayDrop_ProbeFailureFallsBackToRunErr(t *testing.T) {
	client := signaling.New("http://127.0.0.1:1", "test")
	runErr := errors.New("idle timeout")
	got := classifyRelayDrop(context.Background(), client, "sess-1", "tok-1", runErr)
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

// A claimed code may be a stale session whose receiver died: the sender
// Deletes and re-Creates it when it re-enters pairing. joinWithRetry
// must ride out the "already paired" window and join the fresh session.
func TestJoinWithRetry_RetriesWhenStaleSessionReplaced(t *testing.T) {
	_, baseURL := newSignalingTestServer(t)
	senderClient := signaling.New(baseURL, "test")
	deadReceiver := signaling.New(baseURL, "test")
	receiverClient := signaling.New(baseURL, "test")
	code := "abc-defg-jkm"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	created, err := senderClient.Create(ctx, code)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := deadReceiver.Join(ctx, code); err != nil {
		t.Fatalf("first Join: %v", err)
	}

	// Second receiver starts polling against the now-paired session.
	var wg sync.WaitGroup
	var joinErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, joinErr = joinWithRetry(ctx, receiverClient, code, &flags{quiet: true}, nil)
	}()

	// Sender re-enters pairing: stale session out, fresh one in.
	time.Sleep(500 * time.Millisecond)
	if err := senderClient.Delete(ctx, created.SessionID, created.RoleToken); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := senderClient.Create(ctx, code); err != nil {
		t.Fatalf("re-Create: %v", err)
	}

	wg.Wait()
	if joinErr != nil {
		t.Errorf("joinWithRetry should have joined the replacement session, got %v", joinErr)
	}
}

// When the stale session is never replaced, the budget must end with
// the honest E003 — the code really is held by another receiver.
func TestJoinWithRetry_GivesUpOnClaimedAfterBudget(t *testing.T) {
	_, baseURL := newSignalingTestServer(t)
	senderClient := signaling.New(baseURL, "test")
	firstReceiver := signaling.New(baseURL, "test")
	code := "abc-defg-jkm"

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := senderClient.Create(ctx, code); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := firstReceiver.Join(ctx, code); err != nil {
		t.Fatalf("first Join: %v", err)
	}

	orig := joinRetryBudget
	defer func() { joinRetryBudget = orig }()
	joinRetryBudget = 500 * time.Millisecond

	_, err := joinWithRetry(ctx, signaling.New(baseURL, "test"), code, &flags{quiet: true}, nil)
	if !errors.Is(err, fserrors.ErrCodeAlreadyClaimed) {
		t.Errorf("expected ErrCodeAlreadyClaimed after budget, got %v", err)
	}
}
