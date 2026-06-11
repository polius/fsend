package transfer

import (
	"archive/tar"
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
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

	if err := ExtractArchive(arc.Path, dstDir, false); err != nil {
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

// Two top-level args with the same base name would silently merge under
// one tar root and the second's files would clobber the first's on
// extract — both sides reporting success. Must be rejected up front,
// case-insensitively (the receiver may be on macOS/Windows), mirroring
// the multi-file Walk guard.
func TestBuildArchive_RejectsDuplicateRootBasenames(t *testing.T) {
	for _, tc := range []struct{ name, second string }{
		{"exact duplicate", "photos"},
		{"case-insensitive duplicate", "PHOTOS"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			a := filepath.Join(root, "a", "photos")
			b := filepath.Join(root, "b", tc.second)
			for _, d := range []string{a, b} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(d, "pic.jpg"), []byte(d), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := BuildArchive([]string{a, b}, nil)
			if !errors.Is(err, fserrors.ErrUsage) {
				t.Fatalf("BuildArchive(%q, %q) = %v, want ErrUsage", a, b, err)
			}
		})
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

// A FIFO inside a sent folder used to hang BuildArchive forever
// (os.Open on a fifo blocks until a writer appears — before the share
// code was even printed); a socket aborted the whole send. Both must be
// skipped.
func TestBuildArchive_SkipsSpecialFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mkfifo on windows")
	}
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "real.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(srcDir, "pipe.fifo"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan struct{})
	var arc *ArchiveResult
	var err error
	go func() {
		arc, err = BuildArchive([]string{srcDir}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BuildArchive hung on the fifo")
	}
	if err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	defer os.Remove(arc.Path)
	if arc.NumFiles != 1 {
		t.Errorf("NumFiles = %d, want 1 (fifo skipped)", arc.NumFiles)
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

	err = ExtractArchive(tarPath, target, false)
	if err == nil {
		t.Fatalf("expected extraction to fail on path traversal")
	}

	// Verify nothing was written outside target.
	parent := filepath.Dir(target)
	if _, err := os.Stat(filepath.Join(parent, "escape.txt")); err == nil {
		t.Errorf("traversal artifact landed at %s", filepath.Join(parent, "escape.txt"))
	}
}

// TestExtractArchive_RejectsAbsoluteSymlinkSlip guards against tar-slip via
// an absolute symlink: a symlink entry pointing at an absolute path outside
// targetDir, followed by a regular-file entry written *through* that symlink.
// filepath.Join strips the leading slash of an absolute linkname, so a naive
// under-target check passes while os.Symlink still stores the absolute target;
// the follow-up write then lands outside targetDir. Extraction must refuse.
func TestExtractArchive_RejectsAbsoluteSymlinkSlip(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	victim := filepath.Join(root, "victim")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}

	tarPath := filepath.Join(root, "evil.tar")
	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeSymlink, Name: "abslink", Linkname: victim, Mode: 0o777,
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte("PWNED")
	if err := tw.WriteHeader(&tar.Header{
		Typeflag: tar.TypeReg, Name: "abslink/pwned", Mode: 0o644, Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ExtractArchive(tarPath, target, true); err == nil {
		t.Fatal("expected extraction to refuse the absolute-symlink slip")
	}
	if _, err := os.Stat(filepath.Join(victim, "pwned")); err == nil {
		t.Fatalf("tar-slip: wrote through symlink to %s, outside target", filepath.Join(victim, "pwned"))
	}
}

// TestExtractArchive_RejectsPreexistingSymlinkOnOverwrite guards the
// --overwrite path (which skips preflightExtract): a symlink pre-planted
// in the output dir — by a local process or a stale prior run — must not
// be followed when a regular-file entry lands on the same name. Both the
// symlink-at-destination and symlinked-parent shapes are covered.
func TestExtractArchive_RejectsPreexistingSymlinkOnOverwrite(t *testing.T) {
	for _, tc := range []struct {
		name    string
		plant   func(target, victim string) // pre-creates the hostile symlink
		entry   string                      // tar regular-file entry name
		written func(victim string) string  // path that would be clobbered on a follow
	}{
		{
			name: "symlink at destination",
			plant: func(target, victim string) {
				_ = os.Symlink(filepath.Join(victim, "secret"), filepath.Join(target, "clash"))
			},
			entry:   "clash",
			written: func(victim string) string { return filepath.Join(victim, "secret") },
		},
		{
			name:    "symlinked parent dir",
			plant:   func(target, victim string) { _ = os.Symlink(victim, filepath.Join(target, "sub")) },
			entry:   "sub/clash",
			written: func(victim string) string { return filepath.Join(victim, "clash") },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "target")
			victim := filepath.Join(root, "victim")
			if err := os.MkdirAll(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(victim, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.plant(target, victim)

			tarPath := filepath.Join(root, "evil.tar")
			f, err := os.Create(tarPath)
			if err != nil {
				t.Fatal(err)
			}
			tw := tar.NewWriter(f)
			body := []byte("PWNED")
			if err := tw.WriteHeader(&tar.Header{
				Typeflag: tar.TypeReg, Name: tc.entry, Mode: 0o644, Size: int64(len(body)),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(body); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			// overwrite=true skips the preflight; the write itself must not
			// follow the planted link out of target.
			_ = ExtractArchive(tarPath, target, true)
			if b, err := os.ReadFile(tc.written(victim)); err == nil && string(b) == "PWNED" {
				t.Fatalf("wrote through symlink to %s, outside target", tc.written(victim))
			}
		})
	}
}

// TestExtractArchive_ConflictWithoutOverwrite verifies that without
// overwrite=true, a single conflicting file inside the archive causes the
// whole extract to refuse — and that nothing landed on disk in the
// meantime. The companion test confirms overwrite=true clobbers cleanly.
func TestExtractArchive_ConflictWithoutOverwrite(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	// Build an archive with two regular files.
	for rel, content := range map[string][]byte{
		"proj/keeper.txt": []byte("from archive"),
		"proj/clash.txt":  []byte("from archive"),
	} {
		full := filepath.Join(srcDir, rel)
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

	// Pre-place the clashing file at the receiver.
	if err := os.MkdirAll(filepath.Join(dstDir, "proj"), 0o755); err != nil {
		t.Fatal(err)
	}
	clashPath := filepath.Join(dstDir, "proj", "clash.txt")
	if err := os.WriteFile(clashPath, []byte("PREEXISTING"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Without overwrite, the conflict must surface as ErrTargetExists and
	// no file from the archive should land.
	err = ExtractArchive(arc.Path, dstDir, false)
	if !errors.Is(err, fserrors.ErrTargetExists) {
		t.Fatalf("got %v, want ErrTargetExists", err)
	}
	// The pre-existing file is untouched.
	got, err := os.ReadFile(clashPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "PREEXISTING" {
		t.Errorf("clash.txt was modified: got %q", got)
	}
	// The non-conflicting entry was NOT pre-extracted (preflight runs first).
	if _, err := os.Stat(filepath.Join(dstDir, "proj", "keeper.txt")); !os.IsNotExist(err) {
		t.Errorf("keeper.txt landed despite refused extract")
	}

	// With overwrite=true the same archive succeeds and clobbers the
	// conflicting file.
	if err := ExtractArchive(arc.Path, dstDir, true); err != nil {
		t.Fatalf("overwrite extract: %v", err)
	}
	if got, _ := os.ReadFile(clashPath); string(got) != "from archive" {
		t.Errorf("overwrite did not replace clash.txt: %q", got)
	}
}

// A mid-extract failure (disk full, I/O error, truncated tar) must not
// leave a partial file at its final name — that's indistinguishable from
// a complete one. Extraction writes through a sidecar and renames, like
// the non-archive receive path; on failure the sidecar is removed.
func TestExtractArchive_NoFinalFileOnTruncatedStream(t *testing.T) {
	dir := t.TempDir()
	target := t.TempDir()

	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := bytes.Repeat([]byte("x"), 64*1024)
	if err := tw.WriteHeader(&tar.Header{
		Name: "big.bin", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	// Cut off the tail: the header (first 512 bytes) survives but the
	// body doesn't, so io.Copy fails mid-file.
	tarPath := filepath.Join(dir, "trunc.tar")
	if err := os.WriteFile(tarPath, buf.Bytes()[:1024], 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ExtractArchive(tarPath, target, false); err == nil {
		t.Fatal("expected extraction of a truncated tar to fail")
	}
	if _, err := os.Stat(filepath.Join(target, "big.bin")); err == nil {
		t.Error("truncated file landed at its final name")
	}
	if _, err := os.Stat(filepath.Join(target, "big.bin"+partialSuffix)); err == nil {
		t.Error("extraction sidecar not removed on failure")
	}
}

func TestPrefixImohash_StableForIdenticalContent(t *testing.T) {
	// Sanity: identical content of the same length must produce identical
	// PrefixImohash digests. This is the property the resume path relies
	// on — sender and receiver fingerprint the same prefix bytes and the
	// digests have to match.
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
	h1, err := PrefixImohash(p1, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	h2, err := PrefixImohash(p2, int64(len(content)))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Errorf("expected identical files to share PrefixImohash digest")
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

// TestExtractArchive_ReadOnlyDirAndOverwrite covers two extraction
// hazards around restrictive modes: a read-only directory entry must not
// block writing the files inside it (modes are deferred, children before
// parents), and re-extracting over an existing read-only file with
// overwrite consent must replace it instead of failing on O_TRUNC.
func TestExtractArchive_ReadOnlyDirAndOverwrite(t *testing.T) {
	dir := t.TempDir()
	tarPath := filepath.Join(dir, "ro.tar")
	target := t.TempDir()

	f, err := os.Create(tarPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	if err := tw.WriteHeader(&tar.Header{
		Name: "ro-dir/", Mode: 0o555, Typeflag: tar.TypeDir,
	}); err != nil {
		t.Fatal(err)
	}
	body := []byte("inside read-only dir")
	if err := tw.WriteHeader(&tar.Header{
		Name: "ro-dir/file.txt", Mode: 0o444, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if err := ExtractArchive(tarPath, target, false); err != nil {
		t.Fatalf("extract into read-only dir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(target, "ro-dir", "file.txt"))
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("file content after extract: %q, %v", got, err)
	}
	if st, err := os.Stat(filepath.Join(target, "ro-dir")); err != nil || st.Mode().Perm() != 0o555 {
		t.Fatalf("dir mode after extract: %v, %v (want 0555)", st.Mode().Perm(), err)
	}

	// Re-extract over the now read-only file with overwrite consent.
	if err := ExtractArchive(tarPath, target, true); err != nil {
		t.Fatalf("re-extract with overwrite over read-only file: %v", err)
	}

	// Restore writability so t.TempDir cleanup can remove the tree.
	_ = os.Chmod(filepath.Join(target, "ro-dir"), 0o755)
}
