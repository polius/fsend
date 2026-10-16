package main

import (
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/transfer"
)

// The overwrite row must never read as a no-op: the −/+ prefixes carry the
// direction in plain text, and when two rounded sizes collide the row says
// what actually differs.
func TestConflictLabel_DiffDirectionInPlainText(t *testing.T) {
	got := conflictLabel(transfer.Conflict{
		Kind: "differs", LocalSize: 12, IncomingSize: 52,
	}, "small.txt")
	if !strings.Contains(got, "− 12 B") || !strings.Contains(got, "+ 52 B") {
		t.Errorf("row must carry −/+ prefixes: %q", got)
	}
	if strings.Contains(got, "(") {
		t.Errorf("distinct sizes need no tiebreaker: %q", got)
	}
}

func TestConflictLabel_ByteDeltaTiebreaker(t *testing.T) {
	// A one-byte change in a 3.3 MB file renders "3.3 MB → 3.3 MB"; the
	// delta is the only honest signal.
	got := conflictLabel(transfer.Conflict{
		Kind: "differs", LocalSize: 3_300_000, IncomingSize: 3_300_001,
	}, "data.bin")
	if !strings.Contains(got, "(+1 B)") {
		t.Errorf("colliding rounded sizes need a byte delta: %q", got)
	}
}

func TestConflictLabel_MtimeTiebreaker(t *testing.T) {
	// Bytes equal too: the classification rested on mtime, so show the
	// local copy's age.
	got := conflictLabel(transfer.Conflict{
		Kind: "differs", LocalSize: 100, IncomingSize: 100,
		LocalModTime: time.Now().Add(-4 * time.Minute),
	}, "data.bin")
	if !strings.Contains(got, "local copy modified 4m ago") {
		t.Errorf("equal bytes need the mtime tiebreaker: %q", got)
	}
}

func TestConflictLabel_TypeClashKeepsKind(t *testing.T) {
	got := conflictLabel(transfer.Conflict{Kind: "file vs folder"}, "data.bin")
	if !strings.Contains(got, "(file vs folder)") || strings.Contains(got, "→") {
		t.Errorf("type clash row = %q", got)
	}
}

// compactAgo picks the shortest honest unit.
func TestCompactAgo(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{4 * time.Minute, "4m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	} {
		if got := compactAgo(c.d); got != c.want {
			t.Errorf("compactAgo(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
