package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func prefixes(t *testing.T, ss ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

func TestClientIP_TrustedProxies(t *testing.T) {
	s := New(Config{TrustedProxies: prefixes(t, "10.0.0.0/8", "::1/128")})
	for _, tc := range []struct {
		name, remote string
		xff          []string
		want         string
	}{
		{"untrusted peer: header ignored", "198.51.100.7:4000", []string{"203.0.113.1"}, "198.51.100.7"},
		{"trusted peer, no header", "10.1.2.3:4000", nil, "10.1.2.3"},
		{"trusted peer forwards a client", "10.1.2.3:4000", []string{"203.0.113.1"}, "203.0.113.1"},
		{"client-written entries are skipped", "10.1.2.3:4000", []string{"6.6.6.6, 203.0.113.1"}, "203.0.113.1"},
		{"trusted hops are walked past", "10.1.2.3:4000", []string{"203.0.113.1, 10.9.9.9"}, "203.0.113.1"},
		{"repeated headers join", "10.1.2.3:4000", []string{"6.6.6.6", "203.0.113.1"}, "203.0.113.1"},
		{"ports and IPv6", "[::1]:4000", []string{"[2001:db8::5]:1234"}, "2001:db8::5"},
		{"garbled header keeps the proxy's key", "10.1.2.3:4000", []string{"203.0.113.1, nonsense"}, "10.1.2.3"},
		{"all hops trusted", "10.1.2.3:4000", []string{"10.4.4.4"}, "10.1.2.3"},
	} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		for _, v := range tc.xff {
			r.Header.Add("X-Forwarded-For", v)
		}
		if got := s.clientIP(r); got != tc.want {
			t.Errorf("%s: clientIP = %q, want %q", tc.name, got, tc.want)
		}
	}
	plain := New(Config{})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:4000"
	r.Header.Set("X-Forwarded-For", "203.0.113.1")
	if got := plain.clientIP(r); got != "127.0.0.1" {
		t.Errorf("no trusted proxies: clientIP = %q, want the direct peer", got)
	}
}

// The audit's N1 check: a cap of 3 new sessions a minute, five clients
// behind one proxy. Trusted, each client has its own allowance; not
// trusted, they share the proxy's.
func TestRateLimit_BehindProxy(t *testing.T) {
	codes := func(trusted []netip.Prefix) []int {
		s := New(Config{ServerVersion: "0.0.0-test", MaxNewSessionsPerMin: 3, TrustedProxies: trusted})
		srv := httptest.NewServer(s.Handler())
		defer srv.Close()
		var out []int
		for i := 1; i <= 5; i++ {
			body := fmt.Sprintf(`{"client_version":"test","slot":"%032x"}`, i)
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/session", bytes.NewBufferString(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(ProtocolHeader, "3")
			req.Header.Set("X-Forwarded-For", fmt.Sprintf("203.0.113.%d", i))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
			out = append(out, resp.StatusCode)
		}
		return out
	}
	if got := codes(nil); fmt.Sprint(got) != "[200 200 200 429 429]" {
		t.Errorf("untrusted proxy: got %v, want the shared allowance", got)
	}
	if got := codes(prefixes(t, "127.0.0.1/32")); fmt.Sprint(got) != "[200 200 200 200 200]" {
		t.Errorf("trusted proxy: got %v, want each client its own allowance", got)
	}
}

// A sender's own /wait polls don't spend its address's allowance; anyone
// else's /wait still does (it is an existence oracle otherwise).
func TestWait_SenderPollsAreFree(t *testing.T) {
	s := New(Config{ServerVersion: "0.0.0-test", MaxNewSessionsPerMin: 2, LongPollTimeout: 10 * time.Millisecond})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	slot := fmt.Sprintf("%032x", 7)
	created := createSession(t, srv.URL, slot) // spends 1 of 2
	for i := range 5 {
		resp := postJSONAuth(t, srv.URL+"/v1/session/"+slot+"/wait", WaitRequest{}, created.RoleToken)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("sender poll %d: status %d, want 204", i+1, resp.StatusCode)
		}
	}
	other := fmt.Sprintf("%032x", 8)
	createSession(t, srv.URL, other) // the second of 2: still allowed

	resp := postJSONAuth(t, srv.URL+"/v1/session/"+slot+"/wait", WaitRequest{}, "not-the-token")
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("an unauthenticated wait over the budget: status %d, want 429", resp.StatusCode)
	}
}

// A receiver retrying its join with the same nonce (its first response was
// lost) gets the same answer; anyone else still gets 409.
func TestJoin_NonceMakesRetryIdempotent(t *testing.T) {
	srv := newTestServer(t)
	slot := fmt.Sprintf("%032x", 9)
	createSession(t, srv.URL, slot)
	join := func(nonce string) (int, JoinSessionResponse) {
		resp := postJSON(t, srv.URL+"/v1/session/"+slot+"/join", JoinSessionRequest{ClientVersion: "test", Nonce: nonce})
		defer func() { _ = resp.Body.Close() }()
		var out JoinSessionResponse
		if resp.StatusCode == http.StatusOK {
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, out
	}
	code, first := join("nonce-a")
	if code != http.StatusOK {
		t.Fatalf("first join: %d", code)
	}
	code, again := join("nonce-a")
	if code != http.StatusOK || again.RoleToken != first.RoleToken || again.SessionID != first.SessionID ||
		again.YourIceCredentials != first.YourIceCredentials {
		t.Fatalf("retry with the same nonce: %d, %+v; want the first answer %+v", code, again, first)
	}
	for _, other := range []string{"nonce-b", ""} {
		if code, _ := join(other); code != http.StatusConflict {
			t.Errorf("join with nonce %q after pairing: %d, want 409", other, code)
		}
	}
}
