package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProbes_LivenessVsReadiness pins B8: /healthz is pure liveness (always
// 200 while the process is up) and /readyz is readiness (503 while a wired
// relay is a zombie), so an orchestrator can tell "restart me" from "don't
// route to me yet". /health stays the legacy combined alias.
func TestProbes_LivenessVsReadiness(t *testing.T) {
	s := New(Config{ServerVersion: "0.0.0-test"})
	s.WithRelay(&fakeRelay{dead: true}, 443)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	get := func(path string) int {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}

	if got := get("/healthz"); got != http.StatusOK {
		t.Errorf("/healthz status = %d, want 200 (liveness must stay up)", got)
	}
	if got := get("/readyz"); got != http.StatusServiceUnavailable {
		t.Errorf("/readyz status = %d, want 503 (readiness tracks relay health)", got)
	}
	if got := get("/health"); got != http.StatusServiceUnavailable {
		t.Errorf("/health status = %d, want 503 (legacy alias of readiness)", got)
	}
}

// TestSecurityHeaders pins B13: the low-cost hardening headers land on every
// response, including the probe endpoints.
func TestSecurityHeaders(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	want := map[string]string{
		"X-Content-Type-Options":    "nosniff",
		"X-Frame-Options":           "DENY",
		"Strict-Transport-Security": "max-age=31536000",
	}
	for k, w := range want {
		if got := resp.Header.Get(k); got != w {
			t.Errorf("header %s = %q, want %q", k, got, w)
		}
	}
}
