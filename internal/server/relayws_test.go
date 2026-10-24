package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/polius/fsend/internal/relay"
)

// newRelayWSServer wires a real relay (for its WebSocket side) into a
// signaling server and returns a paired session's two role tokens.
func newRelayWSServer(t *testing.T, password string) (ts *httptest.Server, sessionID, senderTok, receiverTok string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s := New(Config{Logger: logger, ServerPassword: password})
	s.WithRelay(relay.NewServer(nil, relay.ServerConfig{Logger: logger}), 4433)
	ts = httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	if password != "" {
		return ts, "", "", ""
	}
	cr := createSession(t, ts.URL, testSlot)
	jresp := postJSON(t, ts.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	defer jresp.Body.Close()
	var jr JoinSessionResponse
	if err := json.NewDecoder(jresp.Body).Decode(&jr); err != nil {
		t.Fatal(err)
	}
	return ts, cr.SessionID, cr.RoleToken, jr.RoleToken
}

func TestRelayAllocate_AdvertisesTCPFallback(t *testing.T) {
	ts, sid, stok, _ := newRelayWSServer(t, "")
	status, body := postRelayAllocate(t, ts.URL, sid, stok)
	if status != http.StatusOK || !body.TCPFallback {
		t.Fatalf("allocate: status %d, tcp_fallback %v; want 200, true", status, body.TCPFallback)
	}

	// A relay without a WebSocket side (an old allocator) doesn't advertise it.
	s := New(metricsConfig())
	s.WithRelay(&fakeRelay{tok: relay.Token{1}}, 443)
	ts2 := httptest.NewServer(s.Handler())
	defer ts2.Close()
	cr := createSession(t, ts2.URL, testSlot)
	if _, body := postRelayAllocate(t, ts2.URL, cr.SessionID, cr.RoleToken); body.TCPFallback {
		t.Error("tcp_fallback advertised by a relay without a WebSocket side")
	}
}

func TestRelayWS_Auth(t *testing.T) {
	ts, sid, stok, rtok := newRelayWSServer(t, "")
	var tok relay.Token
	dial := func(roleToken string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		h := http.Header{}
		h.Set("Authorization", "Bearer "+roleToken)
		c, err := relay.DialWS(ctx, ts.URL+"/v1/relay/ws?session_id="+sid, h, tok, relay.PeerIDFor(roleToken))
		if err == nil {
			_ = c.Close()
		}
		return err
	}
	var se *relay.HTTPStatusError
	if err := dial(stok); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("before allocate: %v, want HTTP 404", err)
	}
	_, body := postRelayAllocate(t, ts.URL, sid, stok)
	tok, _ = relay.ParseToken(body.SessionToken)
	if err := dial("bogus"); !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("bad role token: %v, want HTTP 401", err)
	}
	if err := dial(rtok); err != nil {
		t.Fatalf("authorized peer: %v", err)
	}
}

func TestRelayWS_ProbeNamesRelayAddr(t *testing.T) {
	ts, _, _, _ := newRelayWSServer(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	h, err := relay.ProbeWS(ctx, ts.URL+"/v1/relay/ws", nil)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := h.Get(RelayAddrHeader); got != "127.0.0.1:4433" {
		t.Errorf("%s = %q, want 127.0.0.1:4433", RelayAddrHeader, got)
	}
}

// The WebSocket endpoint sits behind the server password like every other.
func TestRelayWS_ServerPasswordGate(t *testing.T) {
	ts, _, _, _ := newRelayWSServer(t, "s3cret")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var se *relay.HTTPStatusError
	if _, err := relay.ProbeWS(ctx, ts.URL+"/v1/relay/ws", nil); !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("probe without password: %v, want HTTP 401", err)
	}
	h := http.Header{}
	h.Set(AuthHeader, "s3cret")
	if _, err := relay.ProbeWS(ctx, ts.URL+"/v1/relay/ws", h); err != nil {
		t.Fatalf("probe with password: %v", err)
	}
}
