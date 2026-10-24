package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
)

func TestResumeKey_DerivedFromBinding(t *testing.T) {
	a := resumeKey(bytes.Repeat([]byte{1}, 32))
	b := resumeKey(bytes.Repeat([]byte{2}, 32))
	if a == b {
		t.Fatal("different bindings gave the same resume key")
	}
	if a != resumeKey(bytes.Repeat([]byte{1}, 32)) {
		t.Fatal("resumeKey is not deterministic")
	}
	// The rendezvous must look like a code, so discovery treats it as one.
	if err := code.Validate(a.rendezvous); err != nil {
		t.Fatalf("rendezvous %q is not code-shaped: %v", a.rendezvous, err)
	}
	if a.secret == a.rendezvous || len(a.secret) != 64 {
		t.Fatalf("secret %q must be a full-strength value distinct from the rendezvous", a.secret)
	}
}

func useTempConfig(t *testing.T) {
	t.Helper()
	config.SetPathForTesting(filepath.Join(t.TempDir(), "config.json"))
	t.Cleanup(func() { config.SetPathForTesting("") })
}

// A receiver saves its resume key so a rerun of the same command (the
// process died) can still re-pair; once done the key is forgotten.
func TestResumeKey_SaveLoadForget(t *testing.T) {
	useTempConfig(t)
	const c = "abc-defg-jkm"
	if _, ok := loadResumeKey(c); ok {
		t.Fatal("found a key before saving one")
	}
	k := resumeKey([]byte("binding"))
	saveResumeKey(c, k)
	got, ok := loadResumeKey(c)
	if !ok || got != k {
		t.Fatalf("loadResumeKey = %v, %v; want %v", got, ok, k)
	}
	p, _ := resumePath(c)
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("resume file mode: %v, %v", fi, err)
	}
	if rs := newRecvSession(c); !rs.resumed || rs.key != k {
		t.Fatal("a rerun receiver must start from the saved key")
	}
	forgetResumeKey(c)
	if _, ok := loadResumeKey(c); ok {
		t.Fatal("key survived forgetResumeKey")
	}
}

func TestResumeKey_ExpiredIgnored(t *testing.T) {
	useTempConfig(t)
	const c = "abc-defg-jkm"
	p, err := resumePath(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	old := fmt.Sprintf(`{"rendezvous":"abc-defg-hjk","secret":"00","saved":%q}`,
		time.Now().Add(-2*resumeTTL).Format(time.RFC3339))
	if err := os.WriteFile(p, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadResumeKey(c); ok {
		t.Fatal("an expired resume key was used")
	}
}

// The first PAKE switches both sides to the resume key once; later
// pairings keep it, so the receiver only ever needs one.
func TestSessions_SwitchToResumeKeyOnce(t *testing.T) {
	useTempConfig(t)
	const c = "abc-defg-jkm"
	s := &sendSession{key: codeKey(c)}
	r := newRecvSession(c)
	first := []byte("first-binding")
	if s.paired(first) != resumeKey(first) {
		t.Fatal("sender did not switch to the binding-derived key")
	}
	r.paired(first)
	if k, _ := s.current(); k != r.key {
		t.Fatal("sender and receiver derived different resume keys")
	}
	s.paired([]byte("second-binding"))
	r.paired([]byte("second-binding"))
	if k, _ := s.current(); k != resumeKey(first) || r.key != resumeKey(first) {
		t.Fatal("a later pairing changed the resume key")
	}
}

// Failed PAKEs count toward a per-session cap; other errors don't.
func TestSendSession_AuthFailureCap(t *testing.T) {
	s := &sendSession{}
	authErr := fmt.Errorf("handshake: %w", fserrors.ErrPeerAuthFailed)
	for i := 1; i < maxAuthFailures; i++ {
		if err := s.authFailed(authErr); err != nil {
			t.Fatalf("cap hit after %d failures", i)
		}
		if err := s.authFailed(errors.New("timeout")); err != nil {
			t.Fatal("a non-auth error counted toward the cap")
		}
	}
	if err := s.authFailed(authErr); !errors.Is(err, fserrors.ErrTooManyPairingFailures) {
		t.Fatalf("got %v at the cap, want ErrTooManyPairingFailures", err)
	}
}
