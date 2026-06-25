package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// mkSymlink creates a symlink, skipping the test when the platform won't allow
// it (Windows without developer mode), so the suite stays green on the matrix.
func mkSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
}

func bySrcRel(srcs []Source) map[string]Source {
	m := make(map[string]Source, len(srcs))
	for _, s := range srcs {
		m[s.Entry.RelativePath] = s
	}
	return m
}

// A symlink to a sibling file is followed: it travels as a regular file with
// the target's content/size, LinkTarget records the origin, and no symlink
// entry is emitted. The target is still sent under its own name (no dedup).
func TestWalk_FollowsSymlinkToFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkSymlink(t, "file1.txt", filepath.Join(dir, "file2"))

	srcs, err := Walk([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dir)
	by := bySrcRel(srcs)

	f2, ok := by[base+"/file2"]
	if !ok {
		t.Fatalf("file2 missing from walk: %+v", srcs)
	}
	if f2.Entry.Type != wire.EntryFile {
		t.Errorf("file2 type = %v, want EntryFile", f2.Entry.Type)
	}
	if f2.Entry.Size != 5 {
		t.Errorf("file2 size = %d, want 5 (target content)", f2.Entry.Size)
	}
	if f2.LinkTarget != "file1.txt" {
		t.Errorf("file2 LinkTarget = %q, want %q", f2.LinkTarget, "file1.txt")
	}
	if _, ok := by[base+"/file1.txt"]; !ok {
		t.Errorf("target file1.txt should still be sent under its own name (no dedup)")
	}
	for _, s := range srcs {
		if s.Entry.Type == wire.EntrySymlink {
			t.Errorf("walk must not emit symlink entries: %+v", s.Entry)
		}
	}
}

// A symlink to a directory is followed and its contents recursed under the
// link's name.
func TestWalk_FollowsSymlinkToDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "inner.txt"), []byte("xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkSymlink(t, "real", filepath.Join(dir, "alias"))

	srcs, err := Walk([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(dir)
	by := bySrcRel(srcs)
	if d, ok := by[base+"/alias"]; !ok || d.Entry.Type != wire.EntryDir {
		t.Errorf("alias should be a directory entry: %+v", d)
	}
	f, ok := by[base+"/alias/inner.txt"]
	if !ok || f.Entry.Size != 3 {
		t.Fatalf("alias/inner.txt should be sent with the target's content: %+v", f)
	}
	// A file reached through a followed dir-symlink carries its real source so
	// the preview can annotate it "(→ real/inner.txt)".
	if f.LinkTarget != "real/inner.txt" {
		t.Errorf("alias/inner.txt LinkTarget = %q, want %q", f.LinkTarget, "real/inner.txt")
	}
}

// A symlink pointing outside the send set is followed (no inside/outside
// guardrail) — its content is sent.
func TestWalk_FollowsSymlinkOutsideSet(t *testing.T) {
	ext := t.TempDir()
	if err := os.WriteFile(filepath.Join(ext, "secret"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	mkSymlink(t, filepath.Join(ext, "secret"), filepath.Join(dir, "ptr"))

	srcs, err := Walk([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	by := bySrcRel(srcs)
	ptr, ok := by[filepath.Base(dir)+"/ptr"]
	if !ok || ptr.Entry.Type != wire.EntryFile || ptr.Entry.Size != 10 {
		t.Errorf("external symlink should be followed to its 10-byte content: %+v", ptr)
	}
}

func TestWalk_BrokenSymlinkErrors(t *testing.T) {
	dir := t.TempDir()
	mkSymlink(t, "does-not-exist", filepath.Join(dir, "dangling"))

	_, err := Walk([]string{dir}, nil)
	if !errors.Is(err, fserrors.ErrUnsendableSymlink) {
		t.Fatalf("want ErrUnsendableSymlink, got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "dangling") || !strings.Contains(got, "does-not-exist") {
		t.Errorf("error should name the link and target: %q", got)
	}
}

// A symlink that points back into an ancestor directory must be reported as a
// cycle rather than recursing forever.
func TestWalk_CycleErrors(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkSymlink(t, dir, filepath.Join(dir, "sub", "up")) // up -> the root we're walking

	_, err := Walk([]string{dir}, nil)
	if !errors.Is(err, fserrors.ErrUnsendableSymlink) {
		t.Fatalf("want ErrUnsendableSymlink (cycle), got %v", err)
	}
}

// --exclude lets the user send a folder despite an otherwise-fatal broken link.
func TestWalk_ExcludeSkipsBrokenSymlink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	mkSymlink(t, "gone", filepath.Join(dir, "dangling"))

	if _, err := Walk([]string{dir}, []string{"dangling"}); err != nil {
		t.Fatalf("excluding the broken link should allow the send: %v", err)
	}
}
