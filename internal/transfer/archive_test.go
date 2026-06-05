package transfer

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestBuildArchive_RoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	tree := map[string][]byte{
		"src/main.go":      []byte("package main\n"),
		"src/lib/util.go":  bytes.Repeat([]byte("u"), 1024),
		"docs/readme.md":   []byte("# hi\n"),
		"assets/image.bin": bytes.Repeat([]byte{0x99}, 64*1024),
	}
	for rel, content := range tree {
		full := filepath.Join(srcDir, "proj", rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	arc, err := BuildArchive([]string{filepath.Join(srcDir, "proj")}, nil)
	if err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	defer os.Remove(arc.Path)

	if arc.Size <= 0 {
		t.Errorf("archive size = %d, want > 0", arc.Size)
	}
	if arc.NumEntries == 0 {
		t.Errorf("NumEntries = 0")
	}

	if err := ExtractArchive(arc.Path, dstDir); err != nil {
		t.Fatalf("ExtractArchive: %v", err)
	}

	for rel, want := range tree {
		got, err := os.ReadFile(filepath.Join(dstDir, "proj", rel))
		if err != nil {
			t.Errorf("missing %s: %v", rel, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("content mismatch %s", rel)
		}
	}
}

func TestBuildArchive_Deterministic(t *testing.T) {
	// Same inputs must produce identical bytes — that's what makes
	// imohash-based resume work across attempts.
	srcDir := t.TempDir()
	for _, name := range []string{"b.txt", "a.txt", "c.txt"} {
		// Reverse-order writes shouldn't matter; the archive walk sorts.
		if err := os.WriteFile(filepath.Join(srcDir, name), []byte(name+"-body"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	a, err := BuildArchive([]string{srcDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(a.Path)
	b, err := BuildArchive([]string{srcDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(b.Path)

	if a.Blake3Root != b.Blake3Root {
		t.Errorf("non-deterministic archive: blake3 differs across builds")
	}
	if a.Size != b.Size {
		t.Errorf("size differs: %d vs %d", a.Size, b.Size)
	}
}

func TestBuildArchive_ExcludePatterns(t *testing.T) {
	srcDir := t.TempDir()
	for _, p := range []string{
		"proj/keep.go",
		"proj/skip.log",
		"proj/node_modules/dep/index.js",
		"proj/sub/keep.txt",
		"proj/sub/.cache/garbage",
	} {
		full := filepath.Join(srcDir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	arc, err := BuildArchive(
		[]string{filepath.Join(srcDir, "proj")},
		[]string{"*.log", "node_modules", ".cache"},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(arc.Path)

	names := readTarNames(t, arc.Path)
	sort.Strings(names)

	wantPresent := []string{"proj/keep.go", "proj/sub/keep.txt"}
	wantAbsent := []string{
		"proj/skip.log",
		"proj/node_modules/dep/index.js",
		"proj/sub/.cache/garbage",
	}

	for _, want := range wantPresent {
		found := false
		for _, n := range names {
			if n == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in archive, missing", want)
		}
	}
	for _, bad := range wantAbsent {
		for _, n := range names {
			if n == bad {
				t.Errorf("expected %q to be excluded, but found it", bad)
			}
		}
	}
}

func TestExtractArchive_RejectsZipSlip(t *testing.T) {
	// Craft a tar with an absolute-path entry and a traversal entry; both
	// must be rejected before any file is written outside targetDir.
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "evil.tar")
	target := t.TempDir()

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, p := range []string{"../escape.txt", "/etc/oops"} {
		body := []byte("malicious")
		if err := tw.WriteHeader(&tar.Header{
			Name: p, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	err = ExtractArchive(tarPath, target)
	if err == nil {
		t.Fatalf("expected extraction to fail on path traversal")
	}

	// Verify nothing was written outside target.
	parent := filepath.Dir(target)
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Errorf("traversal artifact landed at %s", filepath.Join(parent, "escape.txt"))
	}
}

func TestImohash_PrefixMatchesFull(t *testing.T) {
	// Sanity: for two identical files of the same size, imohash matches.
	dir := t.TempDir()
	content := make([]byte, 4*1024*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}
	p1 := filepath.Join(dir, "a.bin")
	p2 := filepath.Join(dir, "b.bin")
	if err := os.WriteFile(p1, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p2, content, 0o644); err != nil {
		t.Fatal(err)
	}
	h1, err := FileImohash(p1)
	if err != nil {
		t.Fatal(err)
	}
	h2, err := FileImohash(p2)
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("expected identical files to share imohash")
	}

	// PrefixImohash on the full file matches FileImohash.
	hp, err := PrefixImohash(p1, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if hp != h1 {
		t.Errorf("PrefixImohash(full) != FileImohash")
	}
}

// readTarNames lists the entry names in a tar (paths only; not the
// content).
func readTarNames(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tr := tar.NewReader(f)
	var names []string
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		// Trim trailing "/" the tar writer adds to dir entries.
		name := hdr.Name
		if l := len(name); l > 0 && name[l-1] == '/' {
			name = name[:l-1]
		}
		names = append(names, name)
	}
	return names
}
