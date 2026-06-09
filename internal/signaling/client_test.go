package signaling

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
)

func setupServer(t *testing.T) (string, *server.Server) {
	t.Helper()
	s := server.New(server.Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts.URL, s
}

func TestClient_CreateAndJoin_Roundtrip(t *testing.T) {
	url, _ := setupServer(t)
	sender := New(url, "test")
	receiver := New(url, "test")

	ctx := context.Background()

	created, err := sender.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Code == "" || created.SessionID == "" {
		t.Fatal("missing fields in CreateSessionResponse")
	}

	joined, err := receiver.Join(ctx, created.Code)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if joined.SessionID != created.SessionID {
		t.Errorf("session id mismatch: %q vs %q", joined.SessionID, created.SessionID)
	}
}

func TestClient_JoinNonexistent_MapsToFserror(t *testing.T) {
	url, _ := setupServer(t)
	c := New(url, "test")
	_, err := c.Join(context.Background(), "aaa-bbbb-ccc")
	if !errors.Is(err, fserrors.ErrCodeNotFound) {
		t.Errorf("expected ErrCodeNotFound, got %v", err)
	}
}

// TestClient_DoubleJoin_MapsToCodeAlreadyClaimed exercises the same code
// path that gates both the direct and relay receive flows: a second
// receiver hitting Join after the session is already paired must surface
// fserrors.ErrCodeAlreadyClaimed (catalog E003), not a raw HTTP 409.
// Without this, the receiver would land on the E099 catchall.
func TestClient_DoubleJoin_MapsToCodeAlreadyClaimed(t *testing.T) {
	url, _ := setupServer(t)
	sender := New(url, "test")
	receiver1 := New(url, "test")
	receiver2 := New(url, "test")
	ctx := context.Background()

	created, err := sender.Create(ctx, "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := receiver1.Join(ctx, created.Code); err != nil {
		t.Fatalf("Join (first receiver): %v", err)
	}
	_, err = receiver2.Join(ctx, created.Code)
	if !errors.Is(err, fserrors.ErrCodeAlreadyClaimed) {
		t.Errorf("expected ErrCodeAlreadyClaimed for late receiver, got %v", err)
	}
}

func TestClient_WaitPairs(t *testing.T) {
	url, _ := setupServer(t)
	sender := New(url, "test")
	receiver := New(url, "test")
	ctx := context.Background()

	created, err := sender.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}

	// Sender waits in goroutine.
	var wg sync.WaitGroup
	var waitResp string
	var waitErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		r, err := sender.Wait(ctx, created.Code)
		waitErr = err
		if r != nil {
			waitResp = r.PeerObservedAddr
		}
	}()

	time.Sleep(50 * time.Millisecond) // ensure wait is parked
	if _, err := receiver.Join(ctx, created.Code); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if waitErr != nil {
		t.Errorf("wait error: %v", waitErr)
	}
	if waitResp == "" {
		t.Error("expected non-empty PeerObservedAddr after pair")
	}
}

func TestClient_WaitTimesOut(t *testing.T) {
	url, _ := setupServer(t)
	c := New(url, "test")
	created, _ := c.Create(context.Background(), "")
	resp, err := c.Wait(context.Background(), created.Code)
	if err != nil {
		t.Errorf("wait timeout should be (nil, nil), got err: %v", err)
	}
	if resp != nil {
		t.Errorf("wait timeout should be (nil, nil), got resp: %+v", resp)
	}
}

func TestClient_Delete(t *testing.T) {
	url, _ := setupServer(t)
	c := New(url, "test")
	ctx := context.Background()

	created, _ := c.Create(ctx, "")
	if err := c.Delete(ctx, created.SessionID, created.RoleToken); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

// Delete without the session's role token must be rejected, so a third
// party who learns a session ID can't tear down someone else's session.
func TestClient_Delete_RejectsWrongToken(t *testing.T) {
	url, _ := setupServer(t)
	c := New(url, "test")
	ctx := context.Background()

	created, _ := c.Create(ctx, "")
	if err := c.Delete(ctx, created.SessionID, "not-the-token"); err == nil {
		t.Error("Delete with a bogus role token should fail")
	}
}

func TestClient_UnreachableServer_MapsToFserror(t *testing.T) {
	c := New("http://127.0.0.1:1", "test") // intentionally bogus port
	_, err := c.RelayStatus(context.Background(), "sess", "")
	if !errors.Is(err, fserrors.ErrServerUnreachable) {
		t.Errorf("expected ErrServerUnreachable, got %v", err)
	}
}

// A slow server that never responds within the request deadline should
// still surface as ErrServerUnreachable, not bubble up the raw
// context.DeadlineExceeded (which would render as the generic E099
// "Unexpected error" through fserrors.Lookup).
// TestClient_ServerPassword_HappyPath shows that WithPassword sets the
// X-Fsend-Auth header on every outbound call, and that a matching
// server password lets Create + Join through normally.
func TestClient_ServerPassword_HappyPath(t *testing.T) {
	s := server.New(server.Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
		ServerPassword:       "swordfish",
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	c := New(ts.URL, "test").WithPassword("swordfish")
	if _, err := c.Create(context.Background(), ""); err != nil {
		t.Fatalf("Create with correct password: %v", err)
	}
}

// TestClient_ServerPassword_MissingOrWrong covers the two failure
// modes: no password attached, and a wrong password attached. Both
// must surface as fserrors.ErrServerAuthRequired so the catalog
// renders E028 instead of an opaque 401.
func TestClient_ServerPassword_MissingOrWrong(t *testing.T) {
	s := server.New(server.Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
		ServerPassword:       "swordfish",
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	for _, tc := range []struct {
		name string
		pw   string
	}{
		{"no password", ""},
		{"wrong password", "guess"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := New(ts.URL, "test")
			if tc.pw != "" {
				c = c.WithPassword(tc.pw)
			}
			_, err := c.Create(context.Background(), "")
			if !errors.Is(err, fserrors.ErrServerAuthRequired) {
				t.Errorf("got %v, want ErrServerAuthRequired", err)
			}
		})
	}
}

func TestClient_TimeoutMapsToServerUnreachable(t *testing.T) {
	// httptest server that hangs forever — exercises the http.Client
	// timeout path that fires when packets are dropped on the floor.
	hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(hang.Close)

	c := New(hang.URL, "test")
	// Shrink the timeout so the test runs fast.
	c.hc.Timeout = 50 * time.Millisecond

	_, err := c.RelayStatus(context.Background(), "sess", "")
	if !errors.Is(err, fserrors.ErrServerUnreachable) {
		t.Errorf("expected ErrServerUnreachable, got %v", err)
	}
	// User-initiated cancel must still surface as context.Canceled so the
	// CLI can tell "I aborted" apart from "server didn't respond".
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = c.RelayStatus(ctx, "sess", "")
	if !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled ctx should yield context.Canceled, got %v", err)
	}
}
