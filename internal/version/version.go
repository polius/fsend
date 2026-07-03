// Package version exposes build-time injected version metadata.
//
// At release time, the build embeds the semver tag and commit SHA via
// -ldflags. `go install …@vX.Y.Z` builds carry no ldflags but do embed
// the module version in build info, which init recovers. Plain dev
// builds fall back to "dev" / "unknown".
package version

import (
	"regexp"
	"runtime/debug"
	"strings"
)

// Version is the semver string (without the leading "v"), e.g. "0.1.0".
var Version = "dev"

// Commit is the git short SHA of the build.
var Commit = "unknown"

// Date is the build timestamp in RFC3339.
var Date = "unknown"

func init() {
	if Version != "dev" {
		return // ldflags already set it
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		if v := buildInfoVersion(bi.Main.Version); v != "" {
			Version = v
		}
	}
}

// pseudoVersion matches the timestamp-commit tail of a Go module
// pseudo-version — "v1.9.1-0.20260703131912-cbe8109ecebb" or the
// untagged "v0.0.0-20260703131912-cbe8109ecebb".
var pseudoVersion = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}$`)

// buildInfoVersion maps a build-info main-module version onto the
// ldflags convention (goreleaser injects {{.Version}}, which has no
// leading "v"). Returns "" when build info carries nothing usable:
// `go build` from a checkout reports "(devel)" on older Go, and a VCS
// pseudo-version (possibly "+dirty") on current Go — both are dev
// builds with no release to compare against, and must not unlock the
// update check or --update.
func buildInfoVersion(v string) string {
	if v == "" || v == "(devel)" {
		return ""
	}
	if strings.Contains(v, "+") || pseudoVersion.MatchString(v) {
		return ""
	}
	return strings.TrimPrefix(v, "v")
}

// String returns "fsend X.Y.Z (build abc1234, 2026-06-01)" for release
// builds. Dev builds (Commit/Date unset) collapse to "fsend dev" so the
// parenthetical doesn't read "(build unknown, unknown)".
func String() string {
	if Commit == "" || Commit == "unknown" || Date == "" || Date == "unknown" {
		return "fsend " + Version
	}
	return "fsend " + Version + " (build " + Commit + ", " + Date + ")"
}
