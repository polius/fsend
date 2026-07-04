package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func metricsConfig() Config {
	return Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		PairedTTL:            2 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
	}
}

func getJSON(t *testing.T, url string, v any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", url, resp.StatusCode)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// TestMetrics_Shape is the privacy guardrail: every emitted field must be in
// the allowlist, so a future edit can't sneak in per-IP/per-session data.
func TestMetrics_Shape(t *testing.T) {
	s := New(metricsConfig())
	s.WithRelay(&fakeRelay{}, 443)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	var raw map[string]any
	getJSON(t, srv.URL+"/metrics", &raw)

	top := map[string]bool{
		"version": true, "uptime_seconds": true, "sessions_active": true,
		"sessions_created_total": true, "sessions_paired_total": true,
		"sessions_rejected_total": true, "relay": true,
	}
	for k := range raw {
		if !top[k] {
			t.Errorf("unexpected top-level field %q — metrics must stay aggregate-only", k)
		}
	}
	for k := range top {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing field %q", k)
		}
	}
	assertKeys(t, raw["sessions_rejected_total"], "sessions_rejected_total",
		"rate_limit", "concurrency_limit", "unauthorized")
	assertKeys(t, raw["relay"], "relay",
		"forwarding", "healthy", "transfers_active", "transfers_total",
		"transfers_capped_total", "bytes_forwarded_total", "peak_transfer_bytes",
		"budget_bytes_today")
}

func assertKeys(t *testing.T, v any, name string, want ...string) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object", name)
	}
	for _, k := range want {
		if _, ok := m[k]; !ok {
			t.Errorf("%s missing %q", name, k)
		}
	}
	if len(m) != len(want) {
		t.Errorf("%s has %d keys, want %d (no extra fields)", name, len(m), len(want))
	}
}

func TestMetrics_Counters(t *testing.T) {
	cfg := metricsConfig()
	cfg.MaxNewSessionsPerMin = 1 // so a second create from the same IP is rate-limited
	s := New(cfg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	createSession(t, srv.URL, testSlot)
	resp := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{ClientVersion: "test", Slot: testSlot2})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second create status = %d, want 429", resp.StatusCode)
	}

	var m MetricsResponse
	getJSON(t, srv.URL+"/metrics", &m)
	if m.SessionsCreatedTotal != 1 {
		t.Errorf("sessions_created_total = %d, want 1", m.SessionsCreatedTotal)
	}
	if m.SessionsActive != 1 {
		t.Errorf("sessions_active = %d, want 1", m.SessionsActive)
	}
	if m.SessionsRejectedTotal.RateLimit != 1 {
		t.Errorf("sessions_rejected_total.rate_limit = %d, want 1", m.SessionsRejectedTotal.RateLimit)
	}
}

// With a server password set, /metrics is gated like any other endpoint
// (an open server, tested above, serves it publicly for transparency).
func TestMetrics_GatedByPassword(t *testing.T) {
	cfg := metricsConfig()
	cfg.ServerPassword = "secret"
	s := New(cfg)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	// No password → 401, and that rejection is counted.
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("metrics without password = %d, want 401", resp.StatusCode)
	}

	// With the password → 200, and the snapshot reports that one rejection.
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/metrics", nil)
	req.Header.Set(AuthHeader, "secret")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp2.Body.Close() }()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("metrics with password = %d, want 200", resp2.StatusCode)
	}
	var m MetricsResponse
	if err := json.NewDecoder(resp2.Body).Decode(&m); err != nil {
		t.Fatal(err)
	}
	if m.SessionsRejectedTotal.Unauthorized != 1 {
		t.Errorf("sessions_rejected_total.unauthorized = %d, want 1", m.SessionsRejectedTotal.Unauthorized)
	}
}

func TestMetrics_OmitsRelayWhenAbsent(t *testing.T) {
	s := New(metricsConfig())
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	var raw map[string]any
	getJSON(t, srv.URL+"/metrics", &raw)
	if _, ok := raw["relay"]; ok {
		t.Error("relay block must be omitted when no relay is wired")
	}
}
