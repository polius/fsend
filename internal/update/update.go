// Package update implements a best-effort "a newer fsend exists" check.
//
// The passive check never downloads or replaces anything, it only
// reports whether a new release is out. `fsend --update` (cmd/fsend)
// uses FetchInstaller to get the pinned release's verified installer and
// runs it. The result of the release lookup is cached for a day so normal
// runs do no network I/O, and every failure path of the passive check is
// silent — it must never disturb a transfer.
package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/polius/fsend/internal/atomicfile"
	"github.com/polius/fsend/internal/config"
)

// repo is the GitHub repository releases come from. Named once so the
// API endpoint and the human-facing release page can't drift apart.
const repo = "polius/fsend"

// latestURL redirects to the newest release's tag page. It's the same
// plain release redirect the installers use: unlike the REST API it isn't
// rate-limited per address, so --update keeps working behind shared
// egress (offices, CI, CGNAT). GitHub points /latest only at full
// releases, never drafts or pre-releases. A package var so tests can point
// it at a local server.
var latestURL = "https://github.com/" + repo + "/releases/latest"

// noRedirectClient stops at the first answer: the version is read from
// the redirect itself, so following it would only fetch an HTML page.
var noRedirectClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

const (
	cacheFile     = "update-check.json"
	checkInterval = 24 * time.Hour
	// noticeTimeout keeps the passive check from ever stalling a
	// transfer; explicitTimeout gives `fsend --update` a real chance on
	// slow links.
	noticeTimeout   = 2 * time.Second
	explicitTimeout = 15 * time.Second
)

// cache is the on-disk memo of the last lookup. Latest has no leading "v".
type cache struct {
	CheckedAt time.Time `json:"checked_at"`
	Latest    string    `json:"latest"`
}

// Available returns the newest release version (no leading "v") when it
// is newer than current, and ok=false when it is not — up to date,
// opted out, a dev build, or anything went wrong.
//
// Opt out with FSEND_NO_UPDATE_CHECK=1.
func Available(ctx context.Context, current string) (latest string, ok bool) {
	if current == "" || current == "dev" {
		return "", false
	}
	if v := os.Getenv("FSEND_NO_UPDATE_CHECK"); v != "" && v != "0" && v != "false" {
		return "", false
	}
	latest = latestVersion(ctx)
	if latest == "" || !Newer(latest, current) {
		return "", false
	}
	return latest, true
}

// ReleaseURL is the GitHub release page for a version (leading "v"
// optional), where the user can read what changed before updating.
func ReleaseURL(v string) string {
	return "https://github.com/" + repo + "/releases/tag/v" + strings.TrimPrefix(strings.TrimSpace(v), "v")
}

// Latest returns the latest release version (no leading "v") from a
// fresh lookup, bypassing the daily cache: an explicit `fsend --update`
// deserves a current answer. The error says why the lookup failed.
func Latest(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, explicitTimeout)
	defer cancel()
	latest, err := fetchLatest(ctx)
	if err == nil {
		writeCache(cachePath(), cache{CheckedAt: time.Now(), Latest: latest})
	}
	return latest, err
}

// latestVersion returns the latest release version (no leading "v"),
// preferring a fresh cache and only hitting the network once per
// checkInterval. Returns "" if it has no answer.
func latestVersion(ctx context.Context) string {
	path := cachePath()
	if c, ok := readCache(path); ok && time.Since(c.CheckedAt) < checkInterval {
		return c.Latest
	}
	ctx, cancel := context.WithTimeout(ctx, noticeTimeout)
	defer cancel()
	latest, err := fetchLatest(ctx)
	if err != nil {
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

// fetchLatest reads the latest release tag from where latestURL
// redirects (…/releases/tag/v1.2.3). The caller bounds ctx (notice vs
// explicit timeouts differ).
func fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, latestURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "fsend-update-check")

	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", fmt.Errorf("%s answered %s, not a redirect to the release", req.URL.Host, resp.Status)
	}
	loc, err := resp.Location()
	if err != nil {
		return "", fmt.Errorf("%s redirected nowhere", req.URL.Host)
	}
	if !strings.HasSuffix(path.Dir(loc.Path), "/releases/tag") {
		return "", fmt.Errorf("%s redirected to %s, not a release", req.URL.Host, loc.Redacted())
	}
	tag := strings.TrimPrefix(path.Base(loc.Path), "v")
	if _, _, _, ok := parse(tag); !ok {
		return "", fmt.Errorf("latest release tag %q is not a version", tag)
	}
	return tag, nil
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
		// Atomic write so a concurrent read never sees a half-written
		// cache file (which would fail to unmarshal and just re-fetch, but
		// the tmp+rename avoids the torn read entirely).
		_ = atomicfile.Write(path, data, 0o600)
	}
}

// Newer reports whether semver latest is strictly greater than current.
// Pre-release and build metadata are ignored — /latest only returns
// stable tags, and we never want to nag about a dev build's own suffix.
func Newer(latest, current string) bool {
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
