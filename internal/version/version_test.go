package version

import "testing"

// String shapes the user-visible header on --version and --help. Dev
// builds collapse to "fsend dev" because Commit/Date are unset; release
// builds carry the full "(build SHA, DATE)" parenthetical.
func TestString(t *testing.T) {
	orig := struct{ Ver, Commit, Date string }{Version, Commit, Date}
	t.Cleanup(func() { Version, Commit, Date = orig.Ver, orig.Commit, orig.Date })

	cases := []struct {
		name                  string
		version, commit, date string
		want                  string
	}{
		{"dev_default", "dev", "unknown", "unknown", "fsend dev"},
		{"dev_empty_commit", "dev", "", "2026-06-01", "fsend dev"},
		{"dev_empty_date", "dev", "abc1234", "", "fsend dev"},
		{"release", "0.1.0", "abc1234", "2026-06-01", "fsend 0.1.0 (build abc1234, 2026-06-01)"},
	}
	for _, c := range cases {
		Version, Commit, Date = c.version, c.commit, c.date
		if got := String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// buildInfoVersion feeds the `go install` fallback: module versions are
// v-prefixed and must match the ldflags convention (no "v"), while a
// plain `go build` reports "(devel)" and must stay "dev".
func TestBuildInfoVersion(t *testing.T) {
	cases := []struct{ in, want string }{
		{"v1.0.0", "1.0.0"},
		{"v1.0.1-0.20260611000000-abcdef123456", "1.0.1-0.20260611000000-abcdef123456"},
		{"(devel)", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := buildInfoVersion(c.in); got != c.want {
			t.Errorf("buildInfoVersion(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
