package main

import (
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/transfer"
)

// The overwrite row must never read as a no-op: yours → theirs reads in
// plain text, and when two rounded sizes collide the row says what
// actually differs.
func TestConflictLabel_DiffDirectionInPlainText(t *testing.T) {
	got := conflictLabel(transfer.Conflict{
		Kind: "differs", LocalSize: 12, IncomingSize: 52,
	})
	if got != "12 B → 52 B" {
		t.Errorf("row = %q, want local → incoming", got)
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
	})
	if !strings.Contains(got, "(+1 B)") {
		t.Errorf("colliding rounded sizes need a byte delta: %q", got)
	}
}

// Every differing row says which copy is newer — the deciding fact for two
// edits of one document, since "replace" deletes yours.
func TestConflictLabel_SaysWhichIsNewer(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		local, incoming time.Time
		want            string
	}{
		{now, now.Add(-3 * time.Hour), "yours is newer by 3 h"},
		{now.Add(-48 * time.Hour), now, "theirs is newer by 2 d"},
		{now, now.Add(-time.Second), "same age"}, // inside timestamp precision
	} {
		got := conflictLabel(transfer.Conflict{
			Kind: "differs", LocalSize: 16, IncomingSize: 7,
			LocalModTime: c.local, IncomingModTime: c.incoming,
		})
		if !strings.HasSuffix(got, c.want) {
			t.Errorf("row = %q, want it to end %q", got, c.want)
		}
	}
	// No incoming time (an old sender): the local copy's age still shows.
	got := conflictLabel(transfer.Conflict{
		Kind: "differs", LocalSize: 100, IncomingSize: 100,
		LocalModTime: time.Now().Add(-4 * time.Minute),
	})
	if !strings.Contains(got, "yours modified 4 min ago") {
		t.Errorf("row without the sender's time = %q", got)
	}
}

// The warning above the options fires only when every differing local copy
// is newer — the case where "replace" loses work.
func TestAllLocalNewer(t *testing.T) {
	now := time.Now()
	newerMine := transfer.Conflict{Kind: "differs", LocalModTime: now, IncomingModTime: now.Add(-time.Hour)}
	newerTheirs := transfer.Conflict{Kind: "differs", LocalModTime: now.Add(-time.Hour), IncomingModTime: now}
	clash := transfer.Conflict{Kind: "file vs folder"}
	for _, c := range []struct {
		cs   []transfer.Conflict
		want bool
	}{
		{[]transfer.Conflict{newerMine}, true},
		{[]transfer.Conflict{newerMine, clash}, true},
		{[]transfer.Conflict{newerMine, newerTheirs}, false},
		{[]transfer.Conflict{clash}, false},
	} {
		if got := allLocalNewer(c.cs); got != c.want {
			t.Errorf("allLocalNewer(%d rows) = %v, want %v", len(c.cs), got, c.want)
		}
	}
}

func TestConflictLabel_TypeClashKeepsKind(t *testing.T) {
	got := conflictLabel(transfer.Conflict{Kind: "file vs folder"})
	if got != "file vs folder" {
		t.Errorf("type clash row = %q", got)
	}
}

// compactAgo picks the shortest honest unit.
func TestCompactAgo(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30 s"},
		{4 * time.Minute, "4 min"},
		{3 * time.Hour, "3 h"},
		{50 * time.Hour, "2 d"},
	} {
		if got := compactAgo(c.d); got != c.want {
			t.Errorf("compactAgo(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
