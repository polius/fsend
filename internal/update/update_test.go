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

func TestAvailable(t *testing.T) {
	// Redirect cache to a tempdir and the API to a stub server.
	config.SetPathForTesting(filepath.Join(t.TempDir(), "config.json"))
	t.Cleanup(func() { config.SetPathForTesting("") })

	srv := releaseRedirect(t, "v9.9.9")
	latestURL = srv.URL + "/polius/fsend/releases/latest"

	if latest, ok := Available(context.Background(), "0.1.0"); !ok || latest != "9.9.9" {
		t.Fatalf("Available(0.1.0) = (%q, %v), want (\"9.9.9\", true)", latest, ok)
	}
	// Up-to-date: no notice (served version cached, no second fetch needed).
	if latest, ok := Available(context.Background(), "9.9.9"); ok {
		t.Fatalf("Available(9.9.9) = (%q, true), want ok=false", latest)
	}
	// Dev builds never check.
	if latest, ok := Available(context.Background(), "dev"); ok {
		t.Fatalf("Available(dev) = (%q, true), want ok=false", latest)
	}
	// Opt-out.
	t.Setenv("FSEND_NO_UPDATE_CHECK", "1")
	if latest, ok := Available(context.Background(), "0.1.0"); ok {
		t.Fatalf("Available opted out = (%q, true), want ok=false", latest)
	}
}

func TestReleaseURL(t *testing.T) {
	cases := map[string]string{
		"1.14.1":  "https://github.com/polius/fsend/releases/tag/v1.14.1",
		"v1.14.1": "https://github.com/polius/fsend/releases/tag/v1.14.1",
	}
	for in, want := range cases {
		if got := ReleaseURL(in); got != want {
			t.Errorf("ReleaseURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLatest(t *testing.T) {
	config.SetPathForTesting(filepath.Join(t.TempDir(), "config.json"))
	t.Cleanup(func() { config.SetPathForTesting("") })

	srv := releaseRedirect(t, "v2.0.0")
	latestURL = srv.URL + "/polius/fsend/releases/latest"

	// A fresh cache with a stale answer must be bypassed.
	writeCache(cachePath(), cache{CheckedAt: time.Now(), Latest: "1.0.0"})
	latest, err := Latest(context.Background())
	if err != nil || latest != "2.0.0" {
		t.Fatalf("Latest() = (%q, %v), want (\"2.0.0\", nil)", latest, err)
	}
	// And the fresh answer is written back for the passive check.
	if c, ok := readCache(cachePath()); !ok || c.Latest != "2.0.0" {
		t.Errorf("cache after Latest() = (%+v, %v), want Latest 2.0.0", c, ok)
	}

	// Lookup failure reports ok=false, never a stale value.
	srv.Close()
	if latest, err := Latest(context.Background()); err == nil {
		t.Fatalf("Latest() after server close = (%q, nil), want an error", latest)
	}
}

// releaseRedirect stands in for github.com: /releases/latest answers with
// the redirect to the tag page, as GitHub does, and the API is not there.
func releaseRedirect(t *testing.T, tag string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/polius/fsend/releases/latest" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", "/polius/fsend/releases/tag/"+tag)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// What GitHub might answer instead of the redirect: each is a clear
// error, never a version.
func TestFetchLatest_RejectsOtherAnswers(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"rate limited": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTooManyRequests)
		},
		"page instead of redirect": func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("<html>"))
		},
		"redirect elsewhere": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/login?return_to=x")
			w.WriteHeader(http.StatusFound)
		},
		"tag not a version": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/polius/fsend/releases/tag/nightly")
			w.WriteHeader(http.StatusFound)
		},
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			t.Cleanup(srv.Close)
			latestURL = srv.URL + "/polius/fsend/releases/latest"
			if v, err := fetchLatest(context.Background()); err == nil {
				t.Fatalf("fetchLatest() = %q, want an error", v)
			}
		})
	}
}
