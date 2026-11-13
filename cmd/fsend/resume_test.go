package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/quicconn"
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
	// Windows has no POSIX mode bits; the file sits in the user's profile.
	if fi, err := os.Stat(p); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
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

// fakeAccepter replays a script of Accept outcomes.
type fakeAccepter struct{ errs []error }

func (f *fakeAccepter) Accept(context.Context) (*quicconn.AcceptResult, error) {
	if len(f.errs) == 0 {
		return &quicconn.AcceptResult{}, nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return nil, err
}

// Failed PAKEs on the LAN listener close it only when the server path can
// take over; anything short of that keeps accepting. Peers that gave up
// without failing the PAKE don't count.
func TestAcceptCounting_LANCap(t *testing.T) {
	authErr := fmt.Errorf("%w: %w", quicconn.ErrHandshakeFailed, fserrors.ErrPeerAuthFailed)
	gaveUp := fmt.Errorf("%w: timeout", quicconn.ErrHandshakeFailed)
	script := func(auth int) *fakeAccepter {
		f := &fakeAccepter{}
		for range auth {
			f.errs = append(f.errs, authErr, gaveUp)
		}
		return f
	}
	yes := func() bool { return true }
	no := func() bool { return false }

	if _, err := acceptCounting(context.Background(), script(maxLANAuthFailures-1), yes); err != nil {
		t.Fatalf("closed before the cap: %v", err)
	}
	if _, err := acceptCounting(context.Background(), script(maxLANAuthFailures), yes); !errors.Is(err, fserrors.ErrTooManyPairingFailures) {
		t.Fatalf("at the cap with the server up: got %v, want ErrTooManyPairingFailures", err)
	}
	for name, closeAtCap := range map[string]func() bool{"server down": no, "re-accept": nil} {
		if _, err := acceptCounting(context.Background(), script(3*maxLANAuthFailures), closeAtCap); err != nil {
			t.Fatalf("%s: the cap ended accepting: %v", name, err)
		}
	}
	if _, err := acceptCounting(context.Background(), &fakeAccepter{errs: []error{errors.New("listener closed")}}, yes); err == nil {
		t.Fatal("a failed listener was retried")
	}
}
