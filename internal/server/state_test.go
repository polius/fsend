package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/relay"
)

// A pending code survives a restart: saved on shutdown, restored on start,
// and the sender's token still waits and pairs. Paired sessions and IPs are
// not saved, and the file is consumed on load.
func TestState_PendingSessionSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	cfg := Config{LongPollTimeout: 100 * time.Millisecond}

	old := New(cfg)
	oldSrv := httptest.NewServer(old.Handler())
	pending := createSession(t, oldSrv.URL, testSlot)
	_ = createSession(t, oldSrv.URL, testSlot2)
	jr := postJSON(t, oldSrv.URL+"/v1/session/"+testSlot2+"/join", JoinSessionRequest{})
	jr.Body.Close()
	oldSrv.Close()

	if n, err := old.SaveState(path); err != nil || n != 1 {
		t.Fatalf("SaveState = %d, %v; want 1 pending session", n, err)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "127.0.0.1") {
		t.Errorf("snapshot must not hold IPs:\n%s", raw)
	}
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("snapshot mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}

	next := New(cfg)
	if n, err := next.LoadState(path); err != nil || n != 1 {
		t.Fatalf("LoadState = %d, %v; want 1", n, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("snapshot must be deleted once loaded (stat err = %v)", err)
	}
	srv := httptest.NewServer(next.Handler())
	t.Cleanup(srv.Close)

	wait := func() int {
		resp := postJSONAuth(t, srv.URL+"/v1/session/"+testSlot+"/wait", WaitRequest{}, pending.RoleToken)
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := wait(); got != http.StatusNoContent {
		t.Fatalf("restored wait = %d, want 204", got)
	}
	if next.ipCounts["127.0.0.1"] != 1 {
		t.Errorf("restored session must re-learn the sender's rate key: %v", next.ipCounts)
	}
	jr = postJSON(t, srv.URL+"/v1/session/"+testSlot+"/join", JoinSessionRequest{})
	var joined JoinSessionResponse
	_ = json.NewDecoder(jr.Body).Decode(&joined)
	jr.Body.Close()
	if joined.SessionID != pending.SessionID {
		t.Errorf("joined %q, want the restored session %q", joined.SessionID, pending.SessionID)
	}
	if got := wait(); got != http.StatusOK {
		t.Errorf("wait after join = %d, want 200", got)
	}
}

func TestState_DropsExpiredAndMissingFile(t *testing.T) {
	s := New(Config{UnpairedTTL: time.Minute})
	if n, err := s.LoadState(filepath.Join(t.TempDir(), "absent.json")); n != 0 || err != nil {
		t.Fatalf("missing file: %d, %v; want 0, nil", n, err)
	}
	path := filepath.Join(t.TempDir(), "s.json")
	st := stateFile{Version: stateVersion, Sessions: []savedSession{{
		ID: "old", Slot: testSlot, SenderToken: "t", CreatedAt: time.Now().Add(-time.Hour),
	}}}
	data, _ := json.Marshal(st)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if n, err := s.LoadState(path); n != 0 || err != nil {
		t.Errorf("expired session: %d, %v; want 0, nil", n, err)
	}
}

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

// fakeKeeper is a relay that keeps a daily byte count.
type fakeKeeper struct {
	RelayAllocator
	day, used uint64
	restored  []uint64
}

func (k *fakeKeeper) BudgetUsage(time.Time) (int64, uint64) { return int64(k.day), k.used }
func (k *fakeKeeper) RestoreBudget(_ time.Time, day int64, used uint64) bool {
	if uint64(day) != k.day {
		return false
	}
	k.restored = append(k.restored, used)
	return true
}

// A redeploy keeps the relay's spend for the day: the daily cap would
// otherwise start over on every restart.
func TestState_CarriesRelayBudget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	day := uint64(time.Now().UTC().Truncate(24 * time.Hour).Unix())
	old := New(Config{}).WithRelay(&fakeKeeper{day: day, used: 12345}, 443)
	if _, err := old.SaveState(path); err != nil {
		t.Fatal(err)
	}
	keeper := &fakeKeeper{day: day}
	if _, err := New(Config{}).WithRelay(keeper, 443).LoadState(path); err != nil {
		t.Fatal(err)
	}
	if len(keeper.restored) != 1 || keeper.restored[0] != 12345 {
		t.Fatalf("restored %v, want [12345]", keeper.restored)
	}
}
