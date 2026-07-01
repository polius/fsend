package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/relay"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		PairedTTL:            5 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postJSON(t *testing.T, url string, body any) *http.Response {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := http.Post(url, "application/json", &buf)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// Slots are opaque to the server (any 32 lowercase hex chars), so the
// tests use fixed literals instead of paying the argon2id derivation.
const (
	testSlot  = "0123456789abcdef0123456789abcdef"
	testSlot2 = "fedcba9876543210fedcba9876543210"
)

// createSession POSTs /v1/session with the given slot and decodes the
// response. Fails the test on any non-200.
func createSession(t *testing.T, baseURL, slot string) CreateSessionResponse {
	t.Helper()
	resp := postJSON(t, baseURL+"/v1/session", CreateSessionRequest{ClientVersion: "test", Slot: slot})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("create status = %d", resp.StatusCode)
	}
	var body CreateSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestCreateSession_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	body := createSession(t, srv.URL, testSlot)
	if body.SessionID == "" {
		t.Error("expected non-empty session_id")
	}
	if body.TTLSeconds <= 0 {
		t.Errorf("expected positive TTL, got %d", body.TTLSeconds)
	}
}

// Create requires a well-formed client-derived slot. There is no
// server-side code generation to fall back to — the client owns the
// code, the server only ever sees the argon2id slot.
func TestCreateSession_RejectsMissingOrMalformedSlot(t *testing.T) {
	srv := newTestServer(t)
	for _, tc := range []struct {
		name string
		slot string
	}{
		{"missing", ""},
		{"raw code instead of slot", "abc-defg-jkm"},
		{"too short", "0123456789abcdef"},
		{"bad alphabet", "0123456789abcdefXX23456789abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{Slot: tc.slot})
			defer resp.Body.Close()
			if resp.StatusCode != 400 {
				t.Errorf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// A taken slot is a 409 — the client regenerates a fresh code+slot and
// retries. The server must never silently re-key the session: it has no
// code to offer, and the client has already shown one to the user.
func TestCreateSession_TakenSlotConflicts(t *testing.T) {
	srv := newTestServer(t)
	_ = createSession(t, srv.URL, testSlot)

	second := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{Slot: testSlot})
	defer second.Body.Close()
	if second.StatusCode != 409 {
		t.Errorf("duplicate slot: status = %d, want 409", second.StatusCode)
	}

	// A different slot still goes through.
	_ = createSession(t, srv.URL, testSlot2)
}

func TestJoin_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	create := createSession(t, srv.URL, testSlot)

	// Receiver joins via the same slot (derived from the same code).
	joinResp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	defer joinResp.Body.Close()
	if joinResp.StatusCode != 200 {
		t.Fatalf("join status = %d", joinResp.StatusCode)
	}
	var join JoinSessionResponse
	_ = json.NewDecoder(joinResp.Body).Decode(&join)
	if join.SessionID != create.SessionID {
		t.Errorf("session id mismatch: %q vs %q", join.SessionID, create.SessionID)
	}
}

// TestCreateAndJoin_AdvertiseRelayAddr locks in the deferred-allocation
// contract: create/join carry the STUN/relay address so the client can
// gather srflx candidates without allocating a slot. Empty when no relay.
func TestCreateAndJoin_AdvertiseRelayAddr(t *testing.T) {
	t.Run("with relay", func(t *testing.T) {
		s := New(Config{ServerVersion: "0.0.0-test", UnpairedTTL: 2 * time.Second, PairedTTL: 5 * time.Second, MaxSessionsPerIP: 10, MaxNewSessionsPerMin: 100})
		s.WithRelay(&fakeRelay{tok: relay.Token{1}}, 9999)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		create := createSession(t, ts.URL, testSlot)
		if !strings.HasSuffix(create.RelayAddr, ":9999") {
			t.Errorf("create relay_addr = %q, want host:9999", create.RelayAddr)
		}
		if create.RelayForwardingDisabled {
			t.Error("relay_forwarding_disabled should be false when forwarding is on")
		}
		joinResp := postJSON(t, ts.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
		defer joinResp.Body.Close()
		var join JoinSessionResponse
		_ = json.NewDecoder(joinResp.Body).Decode(&join)
		if !strings.HasSuffix(join.RelayAddr, ":9999") {
			t.Errorf("join relay_addr = %q, want host:9999", join.RelayAddr)
		}
	})
	t.Run("relay forwarding disabled", func(t *testing.T) {
		s := New(Config{ServerVersion: "0.0.0-test", UnpairedTTL: 2 * time.Second, PairedTTL: 5 * time.Second, MaxSessionsPerIP: 10, MaxNewSessionsPerMin: 100})
		s.WithRelay(&fakeRelay{tok: relay.Token{1}, noForward: true}, 9999)
		ts := httptest.NewServer(s.Handler())
		defer ts.Close()

		create := createSession(t, ts.URL, testSlot)
		if create.RelayAddr == "" {
			t.Error("relay_addr must still be set in STUN-only mode (it's the STUN server)")
		}
		if !create.RelayForwardingDisabled {
			t.Error("relay_forwarding_disabled should be true in STUN-only mode")
		}
	})
	t.Run("no relay", func(t *testing.T) {
		srv := newTestServer(t)
		if got := createSession(t, srv.URL, testSlot).RelayAddr; got != "" {
			t.Errorf("relay_addr = %q, want empty when no relay wired", got)
		}
	})
}

func TestJoin_NotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestJoin_AlreadyClaimed(t *testing.T) {
	srv := newTestServer(t)
	_ = createSession(t, srv.URL, testSlot)

	r1 := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	r1.Body.Close()
	r2 := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	defer r2.Body.Close()
	if r2.StatusCode != 409 {
		t.Errorf("expected 409 on second join, got %d", r2.StatusCode)
	}
}

func TestWait_PairedReleases(t *testing.T) {
	srv := newTestServer(t)
	_ = createSession(t, srv.URL, testSlot)

	// Sender starts waiting in a goroutine.
	var wg sync.WaitGroup
	var waitStatus int
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/wait", WaitRequest{})
		defer resp.Body.Close()
		waitStatus = resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond) // ensure waiter is parked

	// Receiver joins → wakeup.
	jr := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	jr.Body.Close()

	wg.Wait()
	if waitStatus != 200 {
		t.Errorf("wait status = %d, want 200", waitStatus)
	}
}

func TestWait_Timeout(t *testing.T) {
	srv := newTestServer(t)
	_ = createSession(t, srv.URL, testSlot)

	resp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/wait", WaitRequest{})
	defer resp.Body.Close()
	// LongPollTimeout in test is 500ms.
	if resp.StatusCode != 204 {
		t.Errorf("expected 204 on timeout, got %d", resp.StatusCode)
	}
}

// /wait must sit behind the same per-IP budget as create/join.
// Before this, it answered 404/204 unthrottled for the exact lookup
// join rate-limits — a free existence oracle over the slot space.
func TestWait_UnknownSlotIsRateLimited(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		PairedTTL:            5 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 3,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	notFound, throttled := 0, 0
	for i := 0; i < 6; i++ {
		resp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/wait", WaitRequest{})
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case 404:
			notFound++
		case 429:
			throttled++
		default:
			t.Fatalf("unexpected status %d at i=%d", resp.StatusCode, i)
		}
	}
	if notFound != 3 || throttled != 3 {
		t.Errorf("got %d×404 + %d×429, want 3+3 (probes must hit the budget)", notFound, throttled)
	}
}

// The sender's legitimate poll cadence must never trip the /wait rate
// limit: one Create plus a /wait every LongPollTimeout (25s in prod) is
// ~3 events/min against a 30/min budget — even five concurrent sessions
// behind one NAT stay comfortably under. Simulated against the bucket
// directly so the test covers a full hour without sleeping.
func TestWaitRateLimit_PollCadenceStaysUnderBudget(t *testing.T) {
	s := New(Config{MaxNewSessionsPerMin: 30}) // prod default: 30 new sessions/min, 25s long-poll
	const senders = 5                          // MaxSessionsPerIP default — worst legitimate case
	now := time.Now()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 0; i < senders; i++ {
		if !s.allowNewSession("1.2.3.4", now) {
			t.Fatalf("create #%d throttled", i+1)
		}
	}
	for elapsed := time.Duration(0); elapsed < time.Hour; elapsed += s.cfg.LongPollTimeout {
		for i := 0; i < senders; i++ {
			if !s.allowNewSession("1.2.3.4", now.Add(elapsed)) {
				t.Fatalf("wait poll at +%v throttled — legitimate cadence must never 429", elapsed)
			}
		}
	}
}

func TestHealth(t *testing.T) {
	srv := newTestServer(t)
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Errorf("health status = %d", resp.StatusCode)
	}
	var h HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if h.Status != "ok" {
		t.Errorf("status = %q", h.Status)
	}
}

// TestCandidates_RoutedByRoleToken proves the demux works for two peers
// sharing a public IP (same NAT) — the old source-IP heuristic mis-
// attributed every push to the first arrival and stranded the receiver
// with zero remote candidates. The bearer token decouples routing from
// IP entirely.
func TestCandidates_RoutedByRoleToken(t *testing.T) {
	srv := newTestServer(t)

	c := createSession(t, srv.URL, testSlot)

	jResp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	defer jResp.Body.Close()
	var j JoinSessionResponse
	_ = json.NewDecoder(jResp.Body).Decode(&j)

	push := func(token, candidate string) int {
		body, _ := json.Marshal(CandidatesPushRequest{Candidates: []string{candidate}})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/session/"+c.SessionID+"/candidates", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	pull := func(token string) []string {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/session/"+c.SessionID+"/candidates?since=0", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == 204 {
			return nil
		}
		var out CandidatesPullResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return out.Candidates
	}

	if got := push(c.RoleToken, "sender-cand-1"); got != 204 {
		t.Fatalf("sender push status %d", got)
	}
	if got := push(j.RoleToken, "receiver-cand-1"); got != 204 {
		t.Fatalf("receiver push status %d", got)
	}

	senderSees := pull(c.RoleToken) // should see receiver's
	receiverSees := pull(j.RoleToken)
	if len(senderSees) != 1 || senderSees[0] != "receiver-cand-1" {
		t.Errorf("sender saw %v, want [receiver-cand-1]", senderSees)
	}
	if len(receiverSees) != 1 || receiverSees[0] != "sender-cand-1" {
		t.Errorf("receiver saw %v, want [sender-cand-1]", receiverSees)
	}

	// No token / wrong token → 401.
	body, _ := json.Marshal(CandidatesPushRequest{Candidates: []string{"x"}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/session/"+c.SessionID+"/candidates", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("missing token: status = %d, want 401", resp.StatusCode)
	}
}

// A negative since used to slice theirs[-1:] and panic the handler.
func TestPullCandidates_NegativeSince(t *testing.T) {
	srv := newTestServer(t)

	c := createSession(t, srv.URL, testSlot)

	jResp := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	defer jResp.Body.Close()
	var j JoinSessionResponse
	_ = json.NewDecoder(jResp.Body).Decode(&j)

	body, _ := json.Marshal(CandidatesPushRequest{Candidates: []string{"receiver-cand-1"}})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/session/"+c.SessionID+"/candidates", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+j.RoleToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/v1/session/"+c.SessionID+"/candidates?since=-1", nil)
	req.Header.Set("Authorization", "Bearer "+c.RoleToken)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("since=-1 request failed (handler panic?): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("since=-1: status = %d, want 200", resp.StatusCode)
	}
	var out CandidatesPullResponse
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if len(out.Candidates) != 1 || out.Candidates[0] != "receiver-cand-1" {
		t.Errorf("since=-1 candidates = %v, want [receiver-cand-1]", out.Candidates)
	}
}

func TestDeleteSession(t *testing.T) {
	srv := newTestServer(t)
	c := createSession(t, srv.URL, testSlot)

	// Delete without the role token must be rejected.
	noTok, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/session/"+c.SessionID, nil)
	noTokResp, err := http.DefaultClient.Do(noTok)
	if err != nil {
		t.Fatal(err)
	}
	noTokResp.Body.Close()
	if noTokResp.StatusCode != 401 {
		t.Errorf("delete without token: status = %d, want 401", noTokResp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/session/"+c.SessionID, nil)
	req.Header.Set("Authorization", "Bearer "+c.RoleToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 204 {
		t.Errorf("delete status = %d", resp.StatusCode)
	}

	// Joining the deleted session should 404.
	jr := postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	defer jr.Body.Close()
	if jr.StatusCode != 404 {
		t.Errorf("expected 404 after delete, got %d", jr.StatusCode)
	}
}

// TestServerPassword_Gate verifies that the password-gated handler
// rejects unauthenticated requests with 401, accepts the configured
// password, and always leaves /v1/health open so monitoring works
// without the secret.
func TestServerPassword_Gate(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
		ServerPassword:       "swordfish",
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	post := func(t *testing.T, path string, header string) int {
		t.Helper()
		body, _ := json.Marshal(CreateSessionRequest{Slot: testSlot})
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set(AuthHeader, header)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if got := post(t, "/v1/session", ""); got != 401 {
		t.Errorf("missing X-Fsend-Auth: status = %d, want 401", got)
	}
	if got := post(t, "/v1/session", "wrong"); got != 401 {
		t.Errorf("wrong password: status = %d, want 401", got)
	}
	if got := post(t, "/v1/session", "swordfish"); got != 200 {
		t.Errorf("correct password: status = %d, want 200", got)
	}

	// /v1/health is always open — monitoring must not need the secret.
	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("health without auth: status = %d, want 200", resp.StatusCode)
	}
}

// TestServerPassword_OpenByDefault confirms the default config (no
// password) leaves every endpoint reachable — the public pairing server
// must not accidentally require auth after this feature lands.
func TestServerPassword_OpenByDefault(t *testing.T) {
	srv := newTestServer(t) // no ServerPassword set
	resp := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{Slot: testSlot})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("open server: status = %d, want 200", resp.StatusCode)
	}
}

// fakeRelay implements RelayAllocator with fixed responses, so the
// relay-status handler can be exercised without standing up a UDP relay.
type fakeRelay struct {
	tok         relay.Token
	reason      string
	maxBytes    uint64
	dead        bool
	noForward   bool
	allocErr    error
	allocCalls  int // number of Allocate() invocations
	budgetAfter int // if >0, Allocate reports the budget spent once allocCalls exceeds it
}

func (f *fakeRelay) Allocate() (relay.Token, error) {
	f.allocCalls++
	if f.budgetAfter > 0 && f.allocCalls > f.budgetAfter {
		return relay.Token{}, relay.ErrBudgetExhausted
	}
	return f.tok, f.allocErr
}
func (f *fakeRelay) Status(t relay.Token) string    { return f.reason }
func (f *fakeRelay) MaxBytesPerSession() uint64     { return f.maxBytes }
func (f *fakeRelay) Healthy() bool                  { return !f.dead }
func (f *fakeRelay) Forwarding() bool               { return !f.noForward }
func (f *fakeRelay) Metrics() relay.Metrics {
	return relay.Metrics{Forwarding: !f.noForward, Healthy: !f.dead}
}

// /v1/health must flip to 503/degraded once the relay read loop has died,
// so an orchestrator restarts the container instead of trusting a zombie.
func TestHealth_DegradedWhenRelayDead(t *testing.T) {
	s := New(Config{ServerVersion: "0.0.0-test"})
	s.WithRelay(&fakeRelay{dead: true}, 443)
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var h HealthResponse
	_ = json.NewDecoder(resp.Body).Decode(&h)
	if h.Status != "degraded" {
		t.Fatalf("status = %q, want degraded", h.Status)
	}
}

// /v1/relay/status must echo back the operator-set byte ceiling so the
// CLI can render a concrete error ("server limit 100 MB") instead of a
// generic "limit reached." Cap-hit carries the limit; idle just carries
// the reason (the timeout is fixed, no value to surface).
func TestRelayStatus_IncludesConfiguredLimits(t *testing.T) {
	cases := []struct {
		name      string
		reason    string
		wantBytes uint64
	}{
		{name: "cap_hit", reason: relay.ReasonCapHit, wantBytes: 100 * 1000 * 1000},
		{name: "idle", reason: relay.ReasonIdle},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s := New(Config{
				ServerVersion:        "0.0.0-test",
				UnpairedTTL:          2 * time.Second,
				PairedTTL:            5 * time.Second,
				LongPollTimeout:      500 * time.Millisecond,
				MaxSessionsPerIP:     10,
				MaxNewSessionsPerMin: 100,
			})
			alloc := &fakeRelay{
				tok:      relay.Token{1, 2, 3, 4},
				reason:   c.reason,
				maxBytes: 100 * 1000 * 1000,
			}
			s.WithRelay(alloc, 9999)
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			// Pair: create a session and allocate the relay token so
			// /relay/status has a token to look up.
			cr := createSession(t, ts.URL, testSlot)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/relay/allocate",
				bytes.NewReader(mustJSON(t, RelayAllocateRequest{SessionID: cr.SessionID})))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+cr.RoleToken)
			alloced, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			alloced.Body.Close()
			if alloced.StatusCode != 200 {
				t.Fatalf("allocate: status = %d", alloced.StatusCode)
			}

			statusReq, _ := http.NewRequest(http.MethodGet,
				ts.URL+"/v1/relay/status?session_id="+cr.SessionID, nil)
			statusReq.Header.Set("Authorization", "Bearer "+cr.RoleToken)
			resp, err := http.DefaultClient.Do(statusReq)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body RelayStatusResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.State != "evicted" {
				t.Fatalf("state = %q, want evicted", body.State)
			}
			if body.Reason != c.reason {
				t.Errorf("reason = %q, want %q", body.Reason, c.reason)
			}
			if body.LimitBytes != c.wantBytes {
				t.Errorf("limit_bytes = %d, want %d", body.LimitBytes, c.wantBytes)
			}
		})
	}
}

// TestRelayAllocate_BudgetExhausted checks a spent daily budget surfaces as
// a 200 with the budget_exhausted flag (mirroring forwarding_disabled), so
// the client fails fast with a specific reason instead of reading an opaque
// 5xx as a generic connectivity failure.
func TestRelayAllocate_BudgetExhausted(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		PairedTTL:            5 * time.Second,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
	})
	s.WithRelay(&fakeRelay{tok: relay.Token{1, 2, 3, 4}, allocErr: relay.ErrBudgetExhausted}, 9999)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cr := createSession(t, ts.URL, testSlot)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/relay/allocate",
		bytes.NewReader(mustJSON(t, RelayAllocateRequest{SessionID: cr.SessionID})))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cr.RoleToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body RelayAllocateResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.BudgetExhausted {
		t.Errorf("budget_exhausted = false, want true")
	}
}

// TestRelayAllocate_BothPeersReuseToken locks in the lazy-once contract:
// the relay token is minted on the first peer's allocate and reused for the
// second, so the second peer never re-invokes Allocate. That's why a budget
// spent between the two calls can't refuse the second peer — it gets the
// cached token and converges via the in-flight breaker instead. The fake is
// rigged to report the budget spent on any *second* Allocate; that must
// never fire, proving the reuse.
func TestRelayAllocate_BothPeersReuseToken(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		PairedTTL:            5 * time.Second,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
	})
	fr := &fakeRelay{tok: relay.Token{9, 9, 9}, budgetAfter: 1}
	s.WithRelay(fr, 9999)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cr := createSession(t, ts.URL, testSlot)
	jresp := postJSON(t, ts.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	var jr JoinSessionResponse
	if err := json.NewDecoder(jresp.Body).Decode(&jr); err != nil {
		t.Fatal(err)
	}
	jresp.Body.Close()

	sStatus, sBody := postRelayAllocate(t, ts.URL, cr.SessionID, cr.RoleToken)
	rStatus, rBody := postRelayAllocate(t, ts.URL, jr.SessionID, jr.RoleToken)

	if sStatus != 200 || rStatus != 200 {
		t.Fatalf("allocate status: sender=%d receiver=%d, want 200/200", sStatus, rStatus)
	}
	if sBody.SessionToken == "" {
		t.Fatal("sender got an empty relay token")
	}
	if rBody.SessionToken != sBody.SessionToken {
		t.Errorf("token mismatch: sender=%q receiver=%q (both peers must share one token)",
			sBody.SessionToken, rBody.SessionToken)
	}
	if rBody.BudgetExhausted {
		t.Error("second peer got budget_exhausted; it must reuse the cached token, not re-allocate")
	}
	if fr.allocCalls != 1 {
		t.Errorf("Allocate called %d times, want 1 (second peer must reuse the cached token)", fr.allocCalls)
	}
}

// postRelayAllocate POSTs /v1/relay/allocate as one session peer and returns
// the status and decoded body.
func postRelayAllocate(t *testing.T, baseURL, sessionID, roleToken string) (int, RelayAllocateResponse) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/relay/allocate",
		bytes.NewReader(mustJSON(t, RelayAllocateRequest{SessionID: sessionID})))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+roleToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body RelayAllocateResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// TestRelayAllocate_ForwardingDisabledFlag checks the allocate response
// mirrors the relay's forwarding mode: the flag is set only in stun-only
// mode, and RelayAddr is always populated (it's the STUN server).
func TestRelayAllocate_ForwardingDisabledFlag(t *testing.T) {
	cases := []struct {
		name      string
		noForward bool
		want      bool
	}{
		{name: "forwarding", noForward: false, want: false},
		{name: "stun_only", noForward: true, want: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			s := New(Config{
				ServerVersion:        "0.0.0-test",
				UnpairedTTL:          2 * time.Second,
				PairedTTL:            5 * time.Second,
				MaxSessionsPerIP:     10,
				MaxNewSessionsPerMin: 100,
			})
			s.WithRelay(&fakeRelay{tok: relay.Token{1, 2, 3, 4}, noForward: c.noForward}, 9999)
			ts := httptest.NewServer(s.Handler())
			defer ts.Close()

			cr := createSession(t, ts.URL, testSlot)
			req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/relay/allocate",
				bytes.NewReader(mustJSON(t, RelayAllocateRequest{SessionID: cr.SessionID})))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+cr.RoleToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var body RelayAllocateResponse
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.ForwardingDisabled != c.want {
				t.Errorf("forwarding_disabled = %v, want %v", body.ForwardingDisabled, c.want)
			}
			if body.RelayAddr == "" {
				t.Error("relay_addr must be set even in stun-only mode (it's the STUN server)")
			}
		})
	}
}

// TestRelayStatus_RequiresRoleToken confirms a caller who knows only the
// session_id (no role token) can't probe relay state: the handler returns
// "unknown", indistinguishable from a nonexistent session.
func TestRelayStatus_RequiresRoleToken(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		PairedTTL:            5 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     10,
		MaxNewSessionsPerMin: 100,
	})
	s.WithRelay(&fakeRelay{tok: relay.Token{1, 2, 3, 4}, reason: relay.ReasonCapHit, maxBytes: 1}, 9999)
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	cr := createSession(t, ts.URL, testSlot)
	allocReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/relay/allocate",
		bytes.NewReader(mustJSON(t, RelayAllocateRequest{SessionID: cr.SessionID})))
	allocReq.Header.Set("Authorization", "Bearer "+cr.RoleToken)
	alloced, err := http.DefaultClient.Do(allocReq)
	if err != nil {
		t.Fatal(err)
	}
	alloced.Body.Close()

	// No Authorization header → must not reveal the evicted state.
	resp, err := http.Get(ts.URL + "/v1/relay/status?session_id=" + cr.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body RelayStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.State != "unknown" {
		t.Errorf("unauthorized status probe = %q, want unknown", body.State)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestRateLimitKey nails down the abuse-prevention contract: every IP
// inside one v6 /64 must map to the same key, every v4 host gets its
// own. We deliberately do NOT depend on whether the input was
// shortest-form or canonical (CIDR comparisons should be transparent to
// that). Garbage in (non-IP strings) is preserved verbatim so callers
// that pass arbitrary tokens stay coherent.
func TestRateLimitKey(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// v4: full /32 preserved.
		{"1.2.3.4", "1.2.3.4"},
		{"127.0.0.1", "127.0.0.1"},

		// v6: collapse to /64 (low 64 bits zeroed).
		{"2001:db8:abcd:1234::1", "2001:db8:abcd:1234::/64"},
		{"2001:db8:abcd:1234::dead:beef", "2001:db8:abcd:1234::/64"},
		{"2001:db8:abcd:1234:5678:9abc:def0:1234", "2001:db8:abcd:1234::/64"},

		// v6 loopback: a /128 in ::/0, masked /64 is still ::/0 → "::".
		{"::1", "::/64"},

		// Garbage passes through.
		{"not-an-ip", "not-an-ip"},
		{"", ""},
	}
	for _, c := range cases {
		if got := rateLimitKey(c.in); got != c.want {
			t.Errorf("rateLimitKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestRateLimitKey_V6CollapsesDistinct128s is the empirical contract: two
// different /128s in the same /64 must hit the same rate-limit identity.
// If this regresses, the abuse scenario where one peer rotates its /128
// to bypass MaxNewSessionsPerMin re-opens silently.
func TestRateLimitKey_V6CollapsesDistinct128s(t *testing.T) {
	a := rateLimitKey("2001:db8:abcd:1234::1")
	b := rateLimitKey("2001:db8:abcd:1234:ffff:ffff:ffff:fffe")
	if a != b {
		t.Fatalf("two /128s in the same /64 produced different keys: %q vs %q", a, b)
	}
	// Different /64s must NOT collide.
	c := rateLimitKey("2001:db8:abcd:5678::1")
	if a == c {
		t.Fatalf("different /64s produced the same key: %q", a)
	}
}

// TestRateLimit_V6BypassClosed exercises the end-to-end abuse path: the
// pre-fix server treats every /128 as a fresh identity, so 35 sessions
// from different /128 inside one /64 sailed through. After the fix the
// per-/64 cap (MaxSessionsPerIP) bites at request N+1.
func TestRateLimit_V6BypassClosed(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          2 * time.Second,
		PairedTTL:            5 * time.Second,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     5,
		MaxNewSessionsPerMin: 1000, // we want concurrent-sessions cap to bite, not rate
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	ok, throttled := 0, 0
	for i := 0; i < 20; i++ {
		// Distinct slot per request so the concurrent-sessions cap is
		// what bites, not a slot collision.
		body := mustJSON(t, CreateSessionRequest{Slot: fmt.Sprintf("%032x", i+1)})
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/session",
			bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		// Rotate /128 inside the same /64 — the exact pattern that
		// bypassed the cap before the rateLimitKey fix.
		req.Header.Set("X-Real-IP", "2001:db8:abcd:1234::"+itoaHex(i+1))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		switch resp.StatusCode {
		case 200:
			ok++
		case 429:
			throttled++
		default:
			t.Fatalf("unexpected status %d at i=%d", resp.StatusCode, i)
		}
	}
	if ok != 5 {
		t.Errorf("ok = %d, want 5 (MaxSessionsPerIP must clamp the /64)", ok)
	}
	if throttled != 15 {
		t.Errorf("throttled = %d, want 15", throttled)
	}
}

// TestSessionCaps_ZeroMeansUnlimited locks in the operator escape hatch:
// 0 disables the per-IP concurrency and per-minute rate caps entirely.
// A regression here (e.g. Default() re-mapping 0→5) would silently
// re-impose a limit the operator explicitly removed — or, worse, turn 0
// into "block everything" via the >= comparison.
func TestSessionCaps_ZeroMeansUnlimited(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          time.Hour,
		PairedTTL:            time.Hour,
		LongPollTimeout:      500 * time.Millisecond,
		MaxSessionsPerIP:     0, // unlimited concurrency
		MaxNewSessionsPerMin: 0, // unlimited rate
	})
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// 20 sessions from one IP would clamp to 5 (or 429 on rate) under the
	// defaults; with both caps at 0 every create must succeed.
	const n = 20
	for i := 0; i < n; i++ {
		body := mustJSON(t, CreateSessionRequest{Slot: fmt.Sprintf("%032x", i+1)})
		req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/session", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Real-IP", "203.0.113.7")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("create #%d: status %d, want 200 (caps disabled)", i+1, resp.StatusCode)
		}
	}
}

// TestEvict_PrunesIPBucket locks in the cleanup contract: rate-limit
// buckets whose stamps have all aged out must be evicted, so the map
// doesn't grow unbounded as new IPs touch the server.
func TestEvict_PrunesIPBucket(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          time.Hour,
		PairedTTL:            time.Hour,
		MaxSessionsPerIP:     100,
		MaxNewSessionsPerMin: 100,
	})
	// Seed buckets directly (the public path requires HTTP requests, but
	// the eviction contract is on the data structure).
	now := time.Now()
	s.mu.Lock()
	s.ipBucket["fresh"] = &rateBucket{stamps: []time.Time{now.Add(-10 * time.Second)}}
	s.ipBucket["stale"] = &rateBucket{stamps: []time.Time{now.Add(-2 * time.Minute)}}
	s.ipBucket["mixed"] = &rateBucket{stamps: []time.Time{
		now.Add(-2 * time.Minute),
		now.Add(-5 * time.Second),
	}}
	s.mu.Unlock()

	s.evict(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ipBucket["fresh"]; !ok {
		t.Error("fresh bucket should still be present")
	}
	if _, ok := s.ipBucket["stale"]; ok {
		t.Error("stale bucket should have been evicted")
	}
	if b, ok := s.ipBucket["mixed"]; !ok {
		t.Error("mixed bucket should still be present")
	} else if len(b.stamps) != 1 {
		t.Errorf("mixed bucket stamps = %d, want 1 (the stale one pruned)", len(b.stamps))
	}
}

// TestEvict_AbandonedSession: a waiting session whose sender stopped
// polling /wait is reclaimed after AbandonedTTL (freeing its per-IP
// slot) long before UnpairedTTL; a still-polling one survives.
func TestEvict_AbandonedSession(t *testing.T) {
	s := New(Config{
		ServerVersion:        "0.0.0-test",
		UnpairedTTL:          time.Hour,
		AbandonedTTL:         5 * time.Minute,
		PairedTTL:            time.Hour,
		MaxSessionsPerIP:     100,
		MaxNewSessionsPerMin: 100,
	})
	now := time.Now()
	mk := func(id, slot string, lastSeen time.Time) {
		s.byID[id] = &session{
			ID: id, Slot: slot, State: "waiting",
			SenderRateKey: "1.2.3.4",
			CreatedAt:     now.Add(-10 * time.Minute),
			LastSeen:      lastSeen,
			waiters:       make(chan struct{}),
		}
		s.bySlot[slot] = s.byID[id]
		s.ipCounts["1.2.3.4"]++
	}
	s.mu.Lock()
	mk("alive", testSlot, now.Add(-30*time.Second))
	mk("dead", testSlot2, now.Add(-6*time.Minute))
	s.mu.Unlock()

	s.evict(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byID["alive"]; !ok {
		t.Error("recently-polled session should survive")
	}
	if _, ok := s.byID["dead"]; ok {
		t.Error("abandoned session should have been reclaimed")
	}
	if got := s.ipCounts["1.2.3.4"]; got != 1 {
		t.Errorf("ipCounts = %d, want 1 (dead session's slot freed)", got)
	}
}

func itoaHex(n int) string {
	const hex = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var buf [16]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = hex[n&0xF]
		n >>= 4
	}
	return string(buf[i:])
}
