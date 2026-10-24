package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Default (info) logs must not carry the client IP or the slot; debug may.
func TestAccessLog_PrivateByDefault(t *testing.T) {
	for _, tc := range []struct {
		level   slog.Level
		private bool
	}{{slog.LevelInfo, true}, {slog.LevelDebug, false}} {
		var buf bytes.Buffer
		s := New(Config{
			LongPollTimeout: 100 * time.Millisecond,
			Logger:          slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: tc.level})),
		})
		srv := httptest.NewServer(s.Handler())
		resp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", nil)
		resp.Body.Close()
		srv.Close()

		out := buf.String()
		if !strings.Contains(out, "/v1/session/{slot}/join") {
			t.Errorf("level %v: route pattern missing from log:\n%s", tc.level, out)
		}
		leaks := strings.Contains(out, testSlot) || strings.Contains(out, "127.0.0.1")
		if leaks == tc.private {
			t.Errorf("level %v: leaks IP/slot = %v, want %v:\n%s", tc.level, leaks, !tc.private, out)
		}
	}
}

// Create/join answer 426 to clients without a supported protocol header,
// or below the operator's minimum release; dev builds pass the latter.
func TestClientGate(t *testing.T) {
	s := New(Config{MinClientVersion: "1.5.0"})
	h := s.Handler()
	for i, tc := range []struct {
		proto, ua string
		want      int
	}{
		{"", "fsend/1.6.0", 426},
		{"2", "fsend/1.6.0", 426},
		{"x", "fsend/1.6.0", 426},
		{"3", "fsend/1.4.9", 426},
		{"3", "fsend/1.5.0", 200},
		{"3", "fsend/dev", 200},
		{"4", "fsend/2.0.0", 200},
	} {
		req := httptest.NewRequest(http.MethodPost, "/v1/session",
			strings.NewReader(fmt.Sprintf(`{"slot":"%032x"}`, i+1)))
		if tc.proto != "" {
			req.Header.Set(ProtocolHeader, tc.proto)
		}
		req.Header.Set("User-Agent", tc.ua)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("proto=%q ua=%q: status %d, want %d", tc.proto, tc.ua, rec.Code, tc.want)
		}
	}
}

// /wait hands out the receiver's address: only the sender's token may ask.
func TestWait_RequiresSenderToken(t *testing.T) {
	srv := newTestServer(t)
	created := createSession(t, srv.URL, testSlot)
	jr := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	var joined JoinSessionResponse
	_ = json.NewDecoder(jr.Body).Decode(&joined)
	jr.Body.Close()

	for _, tc := range []struct {
		name, tok string
		want      int
	}{
		{"none", "", 404},
		{"receiver", joined.RoleToken, 404},
		{"bogus", "AAAA", 404},
		{"sender", created.RoleToken, 200},
	} {
		resp := postJSONAuth(t, srv.URL+"/v1/session/"+testSlot+"/wait", WaitRequest{}, tc.tok)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("%s token: status %d, want %d", tc.name, resp.StatusCode, tc.want)
		}
	}
}

// A session reaped while the sender is parked answers 404 (code gone),
// never 410, which clients read as "server retired".
func TestWait_ExpiryIs404(t *testing.T) {
	s := New(Config{LongPollTimeout: 5 * time.Second, UnpairedTTL: time.Millisecond})
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
	s.evict(time.Now())
	if got := <-done; got != http.StatusNotFound {
		t.Errorf("status %d, want 404", got)
	}
}

// Rotating /64s inside one IPv6 /48 hits the site budget (limit × v6SiteFactor).
func TestRateLimit_V6SiteBudget(t *testing.T) {
	const limit = 2
	s := New(Config{MaxNewSessionsPerMin: limit})
	h := s.Handler()
	ok := 0
	for i := range 20 {
		remote := fmt.Sprintf("[2001:db8:abcd:%x::1]:443", i+1) // a fresh /64 each time
		if postCreateDirect(t, h, remote, fmt.Sprintf("%032x", i+1)) == http.StatusOK {
			ok++
		}
	}
	if ok != limit*v6SiteFactor {
		t.Errorf("ok = %d, want %d (the /48 must cap /64 rotation)", ok, limit*v6SiteFactor)
	}
	// Another /48 is unaffected.
	if got := postCreateDirect(t, h, "[2001:db8:ffff::1]:443", fmt.Sprintf("%032x", 99)); got != http.StatusOK {
		t.Errorf("other /48: status %d, want 200", got)
	}
}

// RelayPublicAddr overrides the Host-derived relay/STUN address.
func TestRelayPublicAddr(t *testing.T) {
	s := New(Config{RelayPublicAddr: "relay.example.com:3478"})
	s.WithRelay(&fakeRelay{}, 9999)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	if got := createSession(t, ts.URL, testSlot).RelayAddr; got != "relay.example.com:3478" {
		t.Errorf("relay_addr = %q, want the configured public address", got)
	}
}
