package transfer

import (
	"path/filepath"
	"strings"
	"testing"
)

// FuzzSanitizeRelativePath hammers the anti-traversal gate with arbitrary
// peer-supplied path strings. Any path it *accepts* must be safe to join under
// a target directory: relative, no volume, no ".." segment, and provably
// inside the root. Anything it rejects is fine. It must never panic.
func FuzzSanitizeRelativePath(f *testing.F) {
	for _, s := range []string{
		"", ".", "a", "a/b/c", "a/../b", "a/b/..", "a/./b",
		"../escape", "a/../../escape", "/abs", `\abs`,
		`C:\windows`, `c:/windows`, `\\unc\share`, "//unc/share",
		"föö/bär", "a\x00b", "....//", "..",
		"notes:secret", "CON", "nul.txt", "COM1", "a/aux.c/b", "report.", "report ",
	} {
		f.Add(s)
	}
	absRoot, err := filepath.Abs("fuzzroot")
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, p string) {
		out, err := SanitizeRelativePath(p)
		if err != nil {
			return // rejection is always acceptable
		}
		if filepath.IsAbs(out) {
			t.Fatalf("accepted absolute result %q from %q", out, p)
		}
		if filepath.VolumeName(out) != "" {
			t.Fatalf("accepted volume name in %q from %q", out, p)
		}
		for _, part := range strings.Split(out, string(filepath.Separator)) {
			if part == ".." {
				t.Fatalf("accepted .. segment in %q from %q", out, p)
			}
		}
		// The security property: joined under a target dir, an accepted path
		// must never resolve outside it. (Precise prefix-boundary check — not
		// pathIsUnder, whose ".." prefix test over-matches dotted filenames
		// like "...." that are legitimate, in-bounds names.)
		joined := filepath.Join(absRoot, out)
		if joined != absRoot && !strings.HasPrefix(joined, absRoot+string(filepath.Separator)) {
			t.Fatalf("sanitized %q -> %q escapes the root (%q)", p, out, joined)
		}
	})
}

// TestPathIsUnder covers the escape check, including filenames that merely
// start with two dots (a legitimate in-bounds case the old prefix test wrongly
// flagged as traversal).
func TestPathIsUnder(t *testing.T) {
	root, err := filepath.Abs("root")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		child string
		want  bool
	}{
		{root, true},
		{filepath.Join(root, "a", "b"), true},
		{filepath.Join(root, "..data"), true},        // dotted filename, not a traversal
		{filepath.Join(root, "..config", "x"), true}, // k8s-style nested
		{filepath.Join(root, "...."), true},
		{filepath.Join(root, "..", "etc"), false}, // real traversal
		{filepath.Dir(root), false},               // parent is not under root
	}
	for _, tc := range cases {
		if got := pathIsUnder(tc.child, root); got != tc.want {
			t.Errorf("pathIsUnder(%q, %q) = %v, want %v", tc.child, root, got, tc.want)
		}
	}
}
