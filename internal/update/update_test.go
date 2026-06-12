package update

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/polius/fsend/internal/config"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		latest, current string
		want            bool
	}{
		{"0.2.0", "0.1.0", true},
		{"v0.2.0", "0.1.0", true},
		{"0.1.1", "0.1.0", true},
		{"1.0.0", "0.9.9", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		{"0.1.0", "0.1.0-rc1", false}, // suffix ignored → equal
		{"garbage", "0.1.0", false},
		{"0.1.0", "dev", false},
	}
	for _, c := range cases {
		if got := Newer(c.latest, c.current); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestNotice(t *testing.T) {
	// Redirect cache to a tempdir and the API to a stub server.
	config.SetPathForTesting(filepath.Join(t.TempDir(), "config.json"))
	t.Cleanup(func() { config.SetPathForTesting("") })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v9.9.9"}`))
	}))
	t.Cleanup(srv.Close)
	apiURL = srv.URL

	if msg := Notice(context.Background(), "0.1.0"); msg == "" {
		t.Fatal("expected an upgrade notice for 0.1.0 vs 9.9.9")
	}
	// Up-to-date: no notice (served version cached, no second fetch needed).
	if msg := Notice(context.Background(), "9.9.9"); msg != "" {
		t.Fatalf("expected no notice when current, got %q", msg)
	}
	// Dev builds never check.
	if msg := Notice(context.Background(), "dev"); msg != "" {
		t.Fatalf("expected no notice for dev build, got %q", msg)
	}
	// Opt-out.
	t.Setenv("FSEND_NO_UPDATE_CHECK", "1")
	if msg := Notice(context.Background(), "0.1.0"); msg != "" {
		t.Fatalf("expected no notice when opted out, got %q", msg)
	}
}

func TestLatest(t *testing.T) {
	config.SetPathForTesting(filepath.Join(t.TempDir(), "config.json"))
	t.Cleanup(func() { config.SetPathForTesting("") })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	}))
	t.Cleanup(srv.Close)
	apiURL = srv.URL

	// A fresh cache with a stale answer must be bypassed.
	writeCache(cachePath(), cache{CheckedAt: time.Now(), Latest: "1.0.0"})
	latest, ok := Latest(context.Background())
	if !ok || latest != "2.0.0" {
		t.Fatalf("Latest() = (%q, %v), want (\"2.0.0\", true)", latest, ok)
	}
	// And the fresh answer is written back for the passive check.
	if c, ok := readCache(cachePath()); !ok || c.Latest != "2.0.0" {
		t.Errorf("cache after Latest() = (%+v, %v), want Latest 2.0.0", c, ok)
	}

	// Lookup failure reports ok=false, never a stale value.
	srv.Close()
	if latest, ok := Latest(context.Background()); ok {
		t.Fatalf("Latest() after server close = (%q, true), want ok=false", latest)
	}
}
