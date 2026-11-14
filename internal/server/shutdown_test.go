package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polius/fsend/internal/relay"
)

// ReleaseWaiters frees parked long-polls at once instead of after the timeout.
func TestReleaseWaiters(t *testing.T) {
	s := New(Config{LongPollTimeout: time.Minute})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	created := createSession(t, srv.URL, testSlot)

	done := make(chan int, 1)
	go func() {
		resp := postJSONAuth(t, srv.URL+"/v1/session/"+testSlot+"/wait", WaitRequest{}, created.RoleToken)
		resp.Body.Close()
		done <- resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond)
	s.ReleaseWaiters()
	select {
	case got := <-done:
		if got != http.StatusNoContent {
			t.Errorf("released wait = %d, want 204", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("parked /wait not released")
	}
}

// Allocating while the relay drains is a retryable 503, not a 500.
func TestRelayAllocate_DrainingIs503(t *testing.T) {
	s := New(Config{})
	s.WithRelay(&fakeRelay{allocErr: relay.ErrDraining}, 443)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	created := createSession(t, srv.URL, testSlot)
	code, _ := postRelayAllocate(t, srv.URL, created.SessionID, created.RoleToken)
	if code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", code)
	}
}
