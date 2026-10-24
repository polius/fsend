package signaling

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// A consumed code tombstones its slot: a late receiver hears "already
// claimed" (E003) instead of "no sender" (E002), and nobody can re-create
// the slot meanwhile.
func TestClient_Consume_LateReceiverGetsAlreadyClaimed(t *testing.T) {
	url, _ := setupServer(t)
	sender := New(url, "test")
	ctx := context.Background()

	created, err := sender.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Consume(ctx, created.SessionID, created.RoleToken); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if _, err := New(url, "test").Join(ctx, created.Code); !errors.Is(err, fserrors.ErrCodeAlreadyClaimed) {
		t.Fatalf("late Join: got %v, want ErrCodeAlreadyClaimed", err)
	}
	if _, err := New(url, "test").Create(ctx, created.Code); !errors.Is(err, fserrors.ErrCodeAlreadyClaimed) {
		t.Fatalf("re-Create of a consumed code: got %v, want ErrCodeAlreadyClaimed", err)
	}
}

// A plain Delete leaves no tombstone: the slot reads as never created.
func TestClient_Delete_LeavesNoTombstone(t *testing.T) {
	url, _ := setupServer(t)
	sender := New(url, "test")
	ctx := context.Background()
	created, err := sender.Create(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := sender.Delete(ctx, created.SessionID, created.RoleToken); err != nil {
		t.Fatal(err)
	}
	if _, err := New(url, "test").Join(ctx, created.Code); !errors.Is(err, fserrors.ErrCodeNotFound) {
		t.Fatalf("Join after Delete: got %v, want ErrCodeNotFound", err)
	}
}

// A refused connection says so, instead of the old blanket "(timeout)".
func TestClient_RefusedNamesTheReason(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	_, err = New("http://"+addr, "test").Join(context.Background(), "abc-defg-jkm")
	if !errors.Is(err, fserrors.ErrServerUnreachable) {
		t.Fatalf("got %v, want ErrServerUnreachable", err)
	}
	var ne *NetError
	if !errors.As(err, &ne) || ne.Reason != "connection refused" {
		t.Fatalf("reason = %v, want \"connection refused\"", err)
	}
}

// An untrusted certificate (captive portal, intercepting proxy) maps to
// its own error, not "unreachable".
func TestClient_UntrustedCertificate(t *testing.T) {
	ts := httptest.NewTLSServer(http.NotFoundHandler())
	defer ts.Close()
	_, err := New(ts.URL, "test").Join(context.Background(), "abc-defg-jkm")
	if !errors.Is(err, fserrors.ErrServerCertUntrusted) {
		t.Fatalf("got %v, want ErrServerCertUntrusted", err)
	}
}
