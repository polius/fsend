package signaling

import (
	"context"
	"errors"
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

	created, err := sender.Create(ctx)
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

func TestClient_WaitPairs(t *testing.T) {
	url, _ := setupServer(t)
	sender := New(url, "test")
	receiver := New(url, "test")
	ctx := context.Background()

	created, err := sender.Create(ctx)
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
	created, _ := c.Create(context.Background())
	resp, err := c.Wait(context.Background(), created.Code)
	if err != nil {
		t.Errorf("wait timeout should be (nil, nil), got err: %v", err)
	}
	if resp != nil {
		t.Errorf("wait timeout should be (nil, nil), got resp: %+v", resp)
	}
}

func TestClient_HealthAndDelete(t *testing.T) {
	url, _ := setupServer(t)
	c := New(url, "test")
	ctx := context.Background()

	h, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if h.Status != "ok" {
		t.Errorf("health status = %q", h.Status)
	}

	created, _ := c.Create(ctx)
	if err := c.Delete(ctx, created.SessionID); err != nil {
		t.Errorf("Delete: %v", err)
	}
}

func TestClient_UnreachableServer_MapsToFserror(t *testing.T) {
	c := New("http://127.0.0.1:1", "test") // intentionally bogus port
	_, err := c.Health(context.Background())
	if !errors.Is(err, fserrors.ErrServerUnreachable) {
		t.Errorf("expected ErrServerUnreachable, got %v", err)
	}
}
