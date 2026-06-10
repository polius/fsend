// Package update implements a best-effort "a newer fsend exists" check.
//
// It is deliberately passive: it never downloads or replaces anything, it
// only tells the user a new release is out and how to get it. The result
// of the GitHub lookup is cached for a day so normal runs do no network
// I/O, and every failure path is silent — an update check must never
// disturb a transfer.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/polius/fsend/internal/config"
)

// apiURL is the GitHub "latest release" endpoint. /latest already
// excludes drafts and pre-releases, so we never nag about those. A
// package var so tests can point it at a local server.
var apiURL = "https://api.github.com/repos/polius/fsend/releases/latest"

const (
	cacheFile     = "update-check.json"
	checkInterval = 24 * time.Hour
	httpTimeout   = 2 * time.Second
	// installCmd is the one-liner we point users at — same as the README.
	installCmd = "curl -fsSL https://getfsend.alzina.dev | sh"
)

// cache is the on-disk memo of the last lookup. Latest has no leading "v".
type cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// Notice returns a short upgrade message if a release newer than
// current is available, or "" when up to date, opted out, or anything
// goes wrong. current is the running version (version.Version).
//
// Opt out with FSEND_NO_UPDATE_CHECK=1. Dev builds never check.
func Notice(ctx context.Context, current string) string {
	if current == "" || current == "dev" {
		return ""
	}
	if v := os.Getenv("FSEND_NO_UPDATE_CHECK"); v != "" && v != "0" && v != "false" {
		return ""
	}
	latest := latestVersion(ctx)
	if latest == "" || !newer(latest, current) {
		return ""
	}
	// Two lines: with the install command inline the notice runs past 80
	// columns. The caller indents the first line; match it on the second.
	return fmt.Sprintf("A new fsend is available (%s → %s).\n  Update: %s",
		strings.TrimPrefix(current, "v"), latest, installCmd)
}

// latestVersion returns the latest release version (no leading "v"),
// preferring a fresh cache and only hitting the network once per
// checkInterval. Returns "" if it has no answer.
func latestVersion(ctx context.Context) string {
	path := cachePath()
	if c, ok := readCache(path); ok && time.Since(c.CheckedAt) < checkInterval {
		return c.Latest
	}
	latest, ok := fetchLatest(ctx)
	if !ok {
		// Network failed: stamp the attempt so we don't retry (and re-pay
		// the timeout) on every run for the next interval, and fall back
		// to whatever we last knew.
		if c, had := readCache(path); had {
			writeCache(path, cache{CheckedAt: time.Now(), Latest: c.Latest})
			return c.Latest
		}
		writeCache(path, cache{CheckedAt: time.Now()})
		return ""
	}
	writeCache(path, cache{CheckedAt: time.Now(), Latest: latest})
	return latest
}

// fetchLatest queries the GitHub API for the latest release tag.
func fetchLatest(ctx context.Context) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, httpTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", false
	}
	// GitHub requires a User-Agent; the recommended Accept pins the API
	// version's media type.
	req.Header.Set("User-Agent", "fsend-update-check")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}

	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false
	}
	tag := strings.TrimPrefix(strings.TrimSpace(body.TagName), "v")
	if tag == "" {
		return "", false
	}
	return tag, true
}

func cachePath() string {
	dir, err := config.Dir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, cacheFile)
}

func readCache(path string) (cache, bool) {
	if path == "" {
		return cache{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cache{}, false
	}
	var c cache
	if err := json.Unmarshal(data, &c); err != nil {
		return cache{}, false
	}
	return c, true
}

func writeCache(path string, c cache) {
	if path == "" {
		return
	}
	if data, err := json.Marshal(c); err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o700)
		_ = os.WriteFile(path, data, 0o600)
	}
}

// newer reports whether semver latest is strictly greater than current.
// Pre-release and build metadata are ignored — /latest only returns
// stable tags, and we never want to nag about a dev build's own suffix.
func newer(latest, current string) bool {
	lMaj, lMin, lPat, ok1 := parse(latest)
	cMaj, cMin, cPat, ok2 := parse(current)
	if !ok1 || !ok2 {
		return false
	}
	switch {
	case lMaj != cMaj:
		return lMaj > cMaj
	case lMin != cMin:
		return lMin > cMin
	default:
		return lPat > cPat
	}
}

// parse splits a "MAJOR.MINOR.PATCH" string (optional leading "v",
// optional -pre/+build suffix) into its numeric components.
func parse(v string) (maj, min, pat int, ok bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.SplitN(v, ".", 3)
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var err error
	if maj, err = strconv.Atoi(parts[0]); err != nil {
		return 0, 0, 0, false
	}
	if min, err = strconv.Atoi(parts[1]); err != nil {
		return 0, 0, 0, false
	}
	if pat, err = strconv.Atoi(parts[2]); err != nil {
		return 0, 0, 0, false
	}
	return maj, min, pat, true
}
