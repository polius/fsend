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
