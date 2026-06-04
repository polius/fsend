// Package version exposes build-time injected version metadata.
//
// At release time, the build embeds the semver tag and commit SHA via
// -ldflags. In development, these fall back to "dev" / "unknown".
package version

// Version is the semver string (without the leading "v"), e.g. "0.1.0".
var Version = "dev"

// Commit is the git short SHA of the build.
var Commit = "unknown"

// Date is the build timestamp in RFC3339.
var Date = "unknown"

// String returns "fsend X.Y.Z (build abc1234, 2026-06-01)".
func String() string {
	return "fsend " + Version + " (build " + Commit + ", " + Date + ")"
}
