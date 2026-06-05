package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
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

func TestCreateSession_HappyPath(t *testing.T) {
	srv := newTestServer(t)
	resp := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{ClientVersion: "test"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body CreateSessionResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if body.Code == "" {
		t.Error("expected non-empty code")
	}
	if body.SessionID == "" {
		t.Error("expected non-empty session_id")
	}
	if body.TTLSeconds <= 0 {
		t.Errorf("expected positive TTL, got %d", body.TTLSeconds)
	}
}

// CreateSession honors the client-suggested code when it's well-formed
// and not already in use. This is what lets the sender register on the
// rendezvous server with the same code it has already shown the user
// from the LAN phase — no code change in the artifact, no E002 race for
// the receiver.
func TestCreateSession_AdoptsSuggestedCode(t *testing.T) {
	srv := newTestServer(t)
	suggested := "abc-defg-jkm"
	resp := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{Code: suggested})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code != suggested {
		t.Errorf("server should have adopted suggested code %q, got %q", suggested, body.Code)
	}
}

// When the suggested code is already taken by another live session, the
// server falls back to generation rather than 409-ing. The user has
// already seen and shared the suggestion; surfacing a fresh code in the
// response is preferable to making them retry. Collisions are rare
// enough (~17M codes) that the re-render is an edge case.
func TestCreateSession_TakenSuggestionFallsBackToGenerated(t *testing.T) {
	srv := newTestServer(t)
	suggested := "abc-defg-jkm"

	first := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{Code: suggested})
	defer first.Body.Close()
	var firstBody CreateSessionResponse
	_ = json.NewDecoder(first.Body).Decode(&firstBody)
	if firstBody.Code != suggested {
		t.Fatalf("setup: first Create should have taken the suggested code")
	}

	second := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{Code: suggested})
	defer second.Body.Close()
	if second.StatusCode != 200 {
		t.Fatalf("second Create should succeed (with a fresh code), got status %d", second.StatusCode)
	}
	var secondBody CreateSessionResponse
	_ = json.NewDecoder(second.Body).Decode(&secondBody)
	if secondBody.Code == suggested {
		t.Errorf("server should have generated a fresh code on collision, got %q", secondBody.Code)
	}
	if secondBody.Code == "" {
		t.Error("server returned an empty code")
	}
}

// Malformed suggestions are ignored, not 400'd. The contract is "best
// effort suggestion"; whatever the client sent, the response is always
// a valid, usable code.
func TestCreateSession_InvalidSuggestionFallsBack(t *testing.T) {
	srv := newTestServer(t)
	resp := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{Code: "not a real code"})
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body CreateSessionResponse
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body.Code == "" || body.Code == "not a real code" {
		t.Errorf("server should have generated a fresh code, got %q", body.Code)
	}
}

func TestJoin_HappyPath(t *testing.T) {
	srv := newTestServer(t)

	// Sender creates.
	createResp := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{})
	defer createResp.Body.Close()
	var create CreateSessionResponse
	_ = json.NewDecoder(createResp.Body).Decode(&create)

	// Receiver joins.
	joinResp := postJSON(t, srv.URL+"/v1/session/"+create.Code+"/join", JoinSessionRequest{})
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

func TestJoin_NotFound(t *testing.T) {
	srv := newTestServer(t)
	resp := postJSON(t, srv.URL+"/v1/session/aaa-bbbb-ccc/join", JoinSessionRequest{})
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

func TestJoin_AlreadyClaimed(t *testing.T) {
	srv := newTestServer(t)
	create := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{})
	defer create.Body.Close()
	var c CreateSessionResponse
	_ = json.NewDecoder(create.Body).Decode(&c)

	r1 := postJSON(t, srv.URL+"/v1/session/"+c.Code+"/join", JoinSessionRequest{})
	r1.Body.Close()
	r2 := postJSON(t, srv.URL+"/v1/session/"+c.Code+"/join", JoinSessionRequest{})
	defer r2.Body.Close()
	if r2.StatusCode != 409 {
		t.Errorf("expected 409 on second join, got %d", r2.StatusCode)
	}
}

func TestWait_PairedReleases(t *testing.T) {
	srv := newTestServer(t)
	create := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{})
	defer create.Body.Close()
	var c CreateSessionResponse
	_ = json.NewDecoder(create.Body).Decode(&c)

	// Sender starts waiting in a goroutine.
	var wg sync.WaitGroup
	var waitStatus int
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp := postJSON(t, srv.URL+"/v1/session/"+c.Code+"/wait", WaitRequest{})
		defer resp.Body.Close()
		waitStatus = resp.StatusCode
	}()
	time.Sleep(50 * time.Millisecond) // ensure waiter is parked

	// Receiver joins → wakeup.
	jr := postJSON(t, srv.URL+"/v1/session/"+c.Code+"/join", JoinSessionRequest{})
	jr.Body.Close()

	wg.Wait()
	if waitStatus != 200 {
		t.Errorf("wait status = %d, want 200", waitStatus)
	}
}

func TestWait_Timeout(t *testing.T) {
	srv := newTestServer(t)
	create := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{})
	defer create.Body.Close()
	var c CreateSessionResponse
	_ = json.NewDecoder(create.Body).Decode(&c)

	resp := postJSON(t, srv.URL+"/v1/session/"+c.Code+"/wait", WaitRequest{})
	defer resp.Body.Close()
	// LongPollTimeout in test is 500ms.
	if resp.StatusCode != 204 {
		t.Errorf("expected 204 on timeout, got %d", resp.StatusCode)
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

func TestDeleteSession(t *testing.T) {
	srv := newTestServer(t)
	create := postJSON(t, srv.URL+"/v1/session", CreateSessionRequest{})
	defer create.Body.Close()
	var c CreateSessionResponse
	_ = json.NewDecoder(create.Body).Decode(&c)

	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/v1/session/"+c.SessionID, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 204 {
		t.Errorf("delete status = %d", resp.StatusCode)
	}

	// Joining the deleted session should 404.
	jr := postJSON(t, srv.URL+"/v1/session/"+c.Code+"/join", JoinSessionRequest{})
	defer jr.Body.Close()
	if jr.StatusCode != 404 {
		t.Errorf("expected 404 after delete, got %d", jr.StatusCode)
	}
}
