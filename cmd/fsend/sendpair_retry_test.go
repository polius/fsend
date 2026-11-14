package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
)

func shrinkServerRetry(t *testing.T) {
	t.Helper()
	orig := serverRetryMax
	serverRetryMax = 5 * time.Millisecond
	t.Cleanup(func() { serverRetryMax = orig })
}

// With LAN still pairing, a server outage is reported once and polling
// continues; the code must not go LAN-only for the rest of the session.
func TestWaitForReceiver_PersistentKeepsPolling(t *testing.T) {
	shrinkWaitRetryStep(t)
	shrinkServerRetry(t)
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) <= waitMaxConsecFails+3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	var downs int32
	resp, err := waitForReceiver(context.Background(), signaling.New(ts.URL, "test"), "abc-defg-jkm",
		func(error) { atomic.AddInt32(&downs, 1) })
	if err != nil || resp == nil {
		t.Fatalf("got resp=%v err=%v, want the pairing after the outage", resp, err)
	}
	if atomic.LoadInt32(&downs) == 0 {
		t.Error("the outage was never reported")
	}
}

// A failed Create is retried (and reported) instead of leaving the
// sender LAN-only for the whole session.
func TestCreateWithRetry_RecoversAfterOutage(t *testing.T) {
	shrinkServerRetry(t)
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"session_id":"s1","role_token":"t"}`))
	}))
	defer ts.Close()

	var downs int32
	created, err := createWithRetry(context.Background(), signaling.New(ts.URL, "test"), "abc-defg-jkm",
		func(error) { atomic.AddInt32(&downs, 1) })
	if err != nil || created.SessionID != "s1" {
		t.Fatalf("got %v, %v; want the session once the server is back", created, err)
	}
	if atomic.LoadInt32(&downs) == 0 {
		t.Error("the outage was never reported")
	}
}

// Without a LAN path to fall back on (onDown nil), the first failure is
// the answer, as before; auth errors need the user either way.
func TestCreateWithRetry_FailsFastWhenNotPersistent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	client := signaling.New(ts.URL, "test")
	if _, err := createWithRetry(context.Background(), client, "abc-defg-jkm", nil); !errors.Is(err, fserrors.ErrServerAuthRequired) {
		t.Fatalf("got %v, want ErrServerAuthRequired", err)
	}
	if _, err := createWithRetry(context.Background(), client, "abc-defg-jkm", func(error) {}); !errors.Is(err, fserrors.ErrServerAuthRequired) {
		t.Fatalf("persistent mode retried an auth error: %v", err)
	}
}

// A server restart loses the session; the sender re-creates the same
// client-derived slot rather than dropping to LAN-only — also with the
// server path alone (--mode direct, no onDown), since the server keeps
// nothing across a restart.
func TestPairOverInternet_RecreatesLostSession(t *testing.T) {
	t.Run("persistent", func(t *testing.T) { testRecreatesLostSession(t, func(error) {}) })
	t.Run("server only", func(t *testing.T) { testRecreatesLostSession(t, nil) })
}

func testRecreatesLostSession(t *testing.T, onDown func(error)) {
	_, baseURL := newSignalingTestServer(t)
	var creates, waits int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/session":
			atomic.AddInt32(&creates, 1)
		case strings.HasSuffix(r.URL.Path, "/wait") && atomic.AddInt32(&waits, 1) == 1:
			w.WriteHeader(http.StatusNotFound) // "the server restarted"
			return
		}
		proxyTo(t, baseURL, w, r)
	}))
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := pairOverInternet(ctx, &flags{quiet: true}, &sendRound{key: codeKey("abc-defg-jkm")},
			&config.Config{Server: ts.URL}, onDown, nil)
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&creates) < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	// The first session still exists server-side in this fake, so the
	// re-create may 409; what matters is that it was attempted.
	if got := atomic.LoadInt32(&creates); got < 2 {
		t.Fatalf("Create called %d times, want a re-create after the lost session", got)
	}
}

// proxyTo forwards r to the real test server at base.
func proxyTo(t *testing.T, base string, w http.ResponseWriter, r *http.Request) {
	t.Helper()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, base+r.URL.RequestURI(), r.Body)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, v := range resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// Someone who joins with the right slot but fails the PAKE doesn't send
// the sender back to waiting: the slot comes from the whole code, so they
// hold the slot without the code, and a fresh session would hand them
// another guess. (A receiver that vanishes before the PAKE still does.)
func TestPairOverInternet_FailedPAKEEndsTheRound(t *testing.T) {
	_, baseURL := newSignalingTestServer(t)
	var creates int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/session" {
			atomic.AddInt32(&creates, 1)
		}
		proxyTo(t, baseURL, w, r)
	}))
	defer ts.Close()

	const code = "abc-defg-jkm"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := pairOverInternet(ctx, &flags{quiet: true}, &sendRound{key: codeKey(code)},
			&config.Config{Server: ts.URL}, func(error) {}, nil)
		done <- err
	}()

	rc := signaling.New(ts.URL, "test")
	var joined *server.JoinSessionResponse
	for {
		var err error
		if joined, err = rc.Join(ctx, code); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("join: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	link, err := linkViaInternet(ctx, &flags{quiet: true}, rc, joined)
	if err != nil {
		t.Fatalf("link: %v", err)
	}
	defer link.close()
	if _, err := link.dial(ctx, "xyz-pqrs-tuv"); !errors.Is(err, fserrors.ErrPeerAuthFailed) {
		t.Fatalf("a wrong code: dial = %v, want ErrPeerAuthFailed", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the sender paired with a peer that failed the PAKE")
		}
	case <-ctx.Done():
		t.Fatal("the sender kept waiting after a failed PAKE")
	}
	if got := atomic.LoadInt32(&creates); got != 1 {
		t.Errorf("Create called %d times, want 1: a failed PAKE must not re-open the code", got)
	}
}
