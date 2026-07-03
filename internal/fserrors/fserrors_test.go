package fserrors

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestLookup_KnownSentinel(t *testing.T) {
	entry, ok := Lookup(ErrServerUnreachable)
	if !ok {
		t.Fatal("expected ok=true for known sentinel")
	}
	if entry.Code != "E001" {
		t.Errorf("expected code E001, got %q", entry.Code)
	}
	if entry.Exit != 1 {
		t.Errorf("expected exit 1, got %d", entry.Exit)
	}
}

func TestLookup_WrappedSentinel(t *testing.T) {
	wrapped := fmt.Errorf("dialing host: %w", ErrServerUnreachable)
	entry, ok := Lookup(wrapped)
	if !ok {
		t.Fatal("expected ok=true for wrapped sentinel")
	}
	if entry.Code != "E001" {
		t.Errorf("expected code E001, got %q", entry.Code)
	}
}

func TestLookup_UnknownError(t *testing.T) {
	entry, ok := Lookup(errors.New("something weird"))
	if ok {
		t.Error("expected ok=false for unknown error")
	}
	if entry.Code != "E099" {
		t.Errorf("expected code E099, got %q", entry.Code)
	}
	if entry.Exit != 99 {
		t.Errorf("expected exit 99, got %d", entry.Exit)
	}
	if !strings.Contains(entry.Message, "something weird") {
		t.Errorf("catchall should include underlying error, got %q", entry.Message)
	}
}

func TestLookup_NilError(t *testing.T) {
	_, ok := Lookup(nil)
	if ok {
		t.Error("expected ok=false for nil error")
	}
}

func TestCatalog_UniqueCodes(t *testing.T) {
	// Both the textual code (Exxx) and the numeric exit must be unique.
	// The only exceptions are warnings (Exit 0, e.g. E016) and the
	// SIGINT convention (Exit 130, e.g. E026) — those are allowed to
	// share their special value.
	seenCode := make(map[string]error)
	seenExit := make(map[int]error)
	for sentinel, entry := range catalog {
		if entry.Code == "" {
			t.Errorf("entry for %v has empty Code", sentinel)
		}
		if existing, ok := seenCode[entry.Code]; ok {
			t.Errorf("duplicate Code %q: %v and %v", entry.Code, existing, sentinel)
		}
		seenCode[entry.Code] = sentinel
		if entry.Exit < 0 {
			t.Errorf("negative Exit for %v: %d", sentinel, entry.Exit)
		}
		if entry.Exit == 0 || entry.Exit == 130 {
			continue
		}
		if existing, ok := seenExit[entry.Exit]; ok {
			t.Errorf("duplicate Exit %d: %v and %v", entry.Exit, existing, sentinel)
		}
		seenExit[entry.Exit] = sentinel
	}
}

func TestEntryRender_PrependsCode(t *testing.T) {
	e := Entry{Code: "E001", Message: "Boom.", Action: "Try X."}
	got := e.Render()
	want := "[E001] Boom.\n  Try X."
	if got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestEntryRender_NoActionStillIncludesCode(t *testing.T) {
	e := Entry{Code: "E026", Message: "Cancelled."}
	got := e.Render()
	want := "[E026] Cancelled."
	if got != want {
		t.Errorf("Render() = %q, want %q", got, want)
	}
}

func TestEntryRender_FallsBackWhenNoCode(t *testing.T) {
	e := Entry{Message: "Boom."}
	if got := e.Render(); got != "Boom." {
		t.Errorf("Render() = %q, want %q", got, "Boom.")
	}
}

func TestNewSentinels_LookupAndExit(t *testing.T) {
	cases := []struct {
		err  error
		code string
		exit int
	}{
		{ErrUsage, "E024", 24},
		{ErrSourceNotFound, "E025", 25},
		{ErrUserCancelled, "E026", 130},
		{ErrRelayIdleTimeout, "E029", 29},
		{ErrRelayBudgetExhausted, "E037", 37},
		{ErrManifestWriteFailed, "E038", 38},
	}
	for _, c := range cases {
		entry, ok := Lookup(c.err)
		if !ok {
			t.Errorf("%v: expected catalog match", c.err)
			continue
		}
		if entry.Code != c.code {
			t.Errorf("%v: code = %q, want %q", c.err, entry.Code, c.code)
		}
		if entry.Exit != c.exit {
			t.Errorf("%v: exit = %d, want %d", c.err, entry.Exit, c.exit)
		}
	}
}

// E010 originates on the sender and is mirrored to the receiver, so the base
// (receiver) wording must point at the sender — the receiver can't check the
// sender's file permissions — while ForSender keeps the direct imperative.
func TestReadFailed_RoleSplitWording(t *testing.T) {
	entry, ok := Lookup(ErrReadFailed)
	if !ok {
		t.Fatal("expected catalog match for ErrReadFailed")
	}
	if !strings.Contains(entry.Message, "sender") || !strings.Contains(entry.Action, "Ask them") {
		t.Errorf("receiver wording should attribute the failure to the sender: %q / %q", entry.Message, entry.Action)
	}
	s := entry.ForSender()
	if strings.Contains(s.Message, "sender") {
		t.Errorf("sender wording should be direct, got %q", s.Message)
	}
	if s.Exit != 10 || entry.Exit != 10 {
		t.Errorf("both roles must keep exit 10, got %d/%d", entry.Exit, s.Exit)
	}
}

func TestChain_WalksWrappers(t *testing.T) {
	wrapped := fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", ErrUsage))
	got := Chain(wrapped)
	if len(got) != 3 {
		t.Fatalf("chain length = %d, want 3; got %v", len(got), got)
	}
	if !strings.Contains(got[0], "outer") || !strings.Contains(got[1], "inner") || got[2] != "usage error" {
		t.Errorf("chain mismatch: %v", got)
	}
}

func TestEntryFormat(t *testing.T) {
	e := Entry{Message: "Boom.", Action: "Try X."}
	got := e.Format()
	want := "Boom.\n  Try X."
	if got != want {
		t.Errorf("Format() = %q, want %q", got, want)
	}

	e2 := Entry{Message: "Boom."}
	if e2.Format() != "Boom." {
		t.Errorf("Format() with no Action should be just Message, got %q", e2.Format())
	}
}
