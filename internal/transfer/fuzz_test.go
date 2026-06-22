package transfer

import (
	"path/filepath"
	"runtime"
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

// TestSymlinkEscapes pins the guard that stops a malicious sender from planting
// a symlink that resolves outside the target dir — a classic transfer RCE
// vector with no negative coverage before this.
func TestSymlinkEscapes(t *testing.T) {
	absTarget := "/etc/passwd"
	if runtime.GOOS == "windows" {
		absTarget = `C:\Windows\System32\drivers\etc\hosts`
	}
	cases := []struct {
		name, relPath, linkTarget string
		want                      bool
	}{
		{"absolute target", "link", absTarget, true},
		{"climbs above root", "link", "../../../etc", true},
		{"deep climb", "a/b/link", "../../../../etc", true},
		{"in-bounds sibling", "a/b/link", "../c", false},
		{"in-bounds same dir", "a/link", "c", false},
		{"in-bounds nested", "link", "sub/file", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := symlinkEscapes("/target", tc.relPath, tc.linkTarget); got != tc.want {
				t.Fatalf("symlinkEscapes(/target, %q, %q) = %v, want %v",
					tc.relPath, tc.linkTarget, got, tc.want)
			}
		})
	}
}
