package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/server"
	"github.com/polius/fsend/internal/signaling"
)

// When the server allocates in stun-only mode (forwarding_disabled), the
// client must fail fast with a clear ErrConnectFailed instead of dialing a
// relay that would only drop its datagrams.
func TestAllocAndDialRelay_ForwardingDisabled(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(server.RelayAllocateResponse{
			RelayAddr:          "127.0.0.1:1",
			SessionToken:       "01000000000000000000000000",
			ForwardingDisabled: true,
		})
	}))
	defer ts.Close()

	_, _, err := allocAndDialRelay(context.Background(),
		signaling.New(ts.URL, "test"), "sid", "role")
	if !errors.Is(err, fserrors.ErrConnectFailed) {
		t.Fatalf("err = %v, want ErrConnectFailed", err)
	}
	if !strings.Contains(err.Error(), "forwarding disabled") {
		t.Errorf("err %q should explain relay forwarding is disabled", err)
	}
}
