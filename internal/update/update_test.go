package update

import "testing"

func TestVersionGreater(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.0", "0.1.0", true},
		{"1.0.0", "0.9.9", true},
		{"0.1.1", "0.1.0", true},
		{"0.1.0", "0.1.0", false},
		{"0.1.0", "0.2.0", false},
		{"0.1.0-rc.1", "0.1.0", false}, // pre-release suffix stripped, equal
		{"", "0.1.0", false},
		{"dev", "0.1.0", false},
		{"0.1.0", "dev", false},
		{"0.10.0", "0.9.0", true},
	}
	for _, c := range cases {
		got := versionGreater(c.a, c.b)
		if got != c.want {
			t.Errorf("versionGreater(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNotice(t *testing.T) {
	if Notice("") != "" {
		t.Error("Notice should be empty for empty version")
	}
	got := Notice("0.2.0")
	if got == "" {
		t.Error("Notice should be non-empty for a real version")
	}
}
