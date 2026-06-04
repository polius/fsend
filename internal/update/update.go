// Package update implements the once-per-day GitHub Releases check.
//
// Behavior (per PROJECT_SPEC.md "Update checking"):
//   - 1-second timeout, failures silent
//   - Result cached in the config file for 24 hours
//   - Notice printed after the transfer, not before
//   - Opt-outs via FSEND_NO_UPDATE_CHECK env var or --no-update-check flag
package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/polius/fsend/internal/config"
)

// LatestReleaseURL is the public GitHub Releases API path for the latest tag.
const LatestReleaseURL = "https://api.github.com/repos/polius/fsend/releases/latest"

// Timeout caps the network request for the version check.
const Timeout = 1 * time.Second

// CacheDuration is how long a successful result is reused without
// hitting GitHub again.
const CacheDuration = 24 * time.Hour

// Check returns the latest known version string and whether it is newer
// than `current`. Returns ("", false, nil) if the check should be skipped
// (env var opt-out, recent cache hit with no newer version, network
// failure within the timeout, etc.).
//
// Never returns an error to the user-visible path — it's silent by design.
func Check(current string) (latest string, newer bool) {
	if os.Getenv("FSEND_NO_UPDATE_CHECK") != "" {
		return "", false
	}
	cfg, _ := config.Load()
	if cfg != nil && cfg.LastUpdateCheck != nil && time.Since(*cfg.LastUpdateCheck) < CacheDuration {
		// Use cached value.
		if cfg.LatestKnownVersion != "" && versionGreater(cfg.LatestKnownVersion, current) {
			return cfg.LatestKnownVersion, true
		}
		return "", false
	}

	ctx, cancel := context.WithTimeout(context.Background(), Timeout)
	defer cancel()
	v, err := fetchLatest(ctx)
	if err != nil {
		// Silent failure: do not pollute the user's terminal.
		return "", false
	}
	now := time.Now().UTC()
	if cfg != nil {
		cfg.LastUpdateCheck = &now
		cfg.LatestKnownVersion = v
		_ = config.Save(cfg)
	}
	return v, versionGreater(v, current)
}

// fetchLatest hits the GitHub API and returns the tag_name with the leading "v" stripped.
func fetchLatest(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, LatestReleaseURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", errors.New("non-200 from GitHub releases API")
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	return strings.TrimPrefix(body.TagName, "v"), nil
}

// versionGreater is a tiny semver compare ("0.2.0" > "0.1.0"). Handles
// 3-segment dotted versions only; pre-release tags are ignored. Good
// enough for "is there a newer release" — full semver parsing would
// require a dependency we'd rather not pull in.
func versionGreater(a, b string) bool {
	if a == "" || b == "" || a == "dev" || b == "dev" {
		return false
	}
	ap := strings.SplitN(a, "-", 2)[0]
	bp := strings.SplitN(b, "-", 2)[0]
	as := strings.Split(ap, ".")
	bs := strings.Split(bp, ".")
	for i := 0; i < 3; i++ {
		var av, bv int
		if i < len(as) {
			fmt.Sscanf(as[i], "%d", &av)
		}
		if i < len(bs) {
			fmt.Sscanf(bs[i], "%d", &bv)
		}
		if av > bv {
			return true
		}
		if av < bv {
			return false
		}
	}
	return false
}

// Notice returns the one-line stderr notice ready to be printed after
// the transfer completes. Returns "" when there's nothing to say.
func Notice(latest string) string {
	if latest == "" {
		return ""
	}
	return "A newer version is available: " + latest + " — https://github.com/polius/fsend/releases/latest"
}
