package main

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/uxlog"
)

// After a failed TCP dial on the local network, QUIC to the same address
// is tried only when the TCP attempt gave the peer no test of the code.
func TestTCPAnswerIsFinal(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"no TCP route", errors.New("quicconn: tcp dial: connection refused"), false},
		{"wrong code", fmt.Errorf("%w: auth", fserrors.ErrPeerAuthFailed), true},
		{"other version", fmt.Errorf("%w: v2", fserrors.ErrIncompatibleVersion), true},
		{"data connection after a good PAKE", fmt.Errorf("%w: %w", quicconn.ErrTCPData, fserrors.ErrPeerAuthFailed), false},
		{"tag unanswered", fmt.Errorf("%w: %w", quicconn.ErrUnconfirmed, io.EOF), true},
	}
	for _, c := range cases {
		if got := tcpAnswerIsFinal(c.err); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// The receiver names the real place: home from "~", the current folder in
// full, and a long path with its middle folders folded into "…".
func TestPlacePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Chdir(home)
	sep := string(filepath.Separator)
	if got := saveTargetLabel(home); got != "~"+sep {
		t.Errorf("current folder = home: got %q, want ~/", got)
	}
	if got := placePath(filepath.Join(home, "Downloads", "report.pdf")); got != "~"+sep+filepath.Join("Downloads", "report.pdf") {
		t.Errorf("under home: got %q", got)
	}
	deep := filepath.Join(home, "projects", "a-rather-long-client-name", "deliverables", "2026", "november", "final")
	got := placePath(deep)
	if uxlog.Width(got) > placeMax || !strings.HasPrefix(got, "~"+sep+"…"+sep) || !strings.HasSuffix(got, sep+"final") {
		t.Errorf("long path: got %q (%d columns)", got, uxlog.Width(got))
	}
	if got := placePath("."); got != "~" {
		t.Errorf(`"." in home: got %q, want ~`, got)
	}
}
