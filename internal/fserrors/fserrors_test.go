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
	if entry.Exit != 2 {
		t.Errorf("expected exit 2, got %d", entry.Exit)
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
	// Codes must be unique (each is a distinct catalog entry).
	// Exit codes may overlap across closely-related entries (per
	// docs/ux/help-text.md: E002 and E003 both exit 3 — "session not found"
	// family). We check codes for uniqueness, but only sanity-check exits.
	seenCode := make(map[string]error)
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
	}
}

func TestIsWarning(t *testing.T) {
	if !IsWarning(ErrConfigCorrupted) {
		t.Error("ErrConfigCorrupted should be a warning")
	}
	if IsWarning(ErrServerUnreachable) {
		t.Error("ErrServerUnreachable should not be a warning")
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
