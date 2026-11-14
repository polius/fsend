package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// placeCase makes a partial and returns it with a target path beside it.
func placeCase(t *testing.T, dir, name string) (src, dst string) {
	t.Helper()
	src = filepath.Join(dir, name+".partial")
	writeFile(t, src, []byte("incoming "+name))
	return src, filepath.Join(dir, name)
}

// placeNew moves the partial onto an absent name, and refuses — leaving
// both files as they were — when anything already holds the name: a file,
// a folder, or a link (even a dangling one).
func TestPlaceNew_OnlyOntoAnAbsentName(t *testing.T) {
	for _, mode := range []string{"exclusive rename", "link fallback"} {
		t.Run(mode, func(t *testing.T) {
			pl := &placer{noExcl: mode == "link fallback"}
			dir := t.TempDir()

			src, dst := placeCase(t, dir, "new.txt")
			if err := pl.placeNew(src, dst); err != nil {
				t.Fatalf("placeNew onto an absent name = %v", err)
			}
			if got := string(mustRead(t, dst)); got != "incoming new.txt" {
				t.Errorf("target = %q", got)
			}
			if _, err := os.Lstat(src); !os.IsNotExist(err) {
				t.Errorf("the partial is still there (%v)", err)
			}

			occupied := map[string]func(string){
				"file.txt": func(p string) { writeFile(t, p, []byte("theirs")) },
				"folder":   func(p string) { _ = os.Mkdir(p, 0o755) },
			}
			if runtime.GOOS != "windows" { // symlink creation needs privileges there
				occupied["dangling"] = func(p string) { _ = os.Symlink(filepath.Join(dir, "nowhere"), p) }
			}
			for name, occupy := range occupied {
				src, dst := placeCase(t, dir, name)
				occupy(dst)
				before, err := os.Lstat(dst)
				if err != nil {
					t.Fatal(err)
				}
				if err := pl.placeNew(src, dst); !errors.Is(err, errTargetAppeared) {
					t.Errorf("%s: placeNew = %v, want errTargetAppeared", name, err)
				}
				if after, err := os.Lstat(dst); err != nil || after.Mode() != before.Mode() || !os.SameFile(before, after) {
					t.Errorf("%s: what held the name was replaced", name)
				}
				if got := string(mustRead(t, src)); got != "incoming "+name {
					t.Errorf("%s: the partial was consumed (%q)", name, got)
				}
			}
			if mode == "exclusive rename" && haveRenameNoReplace && pl.noExcl {
				t.Error("the exclusive rename was given up on a filesystem that has it")
			}
		})
	}
}

// A filesystem without the exclusive rename is probed once, not once per
// file; the files are still placed, and an existing name still refused.
// Any other failure of the call uses the fallback for that file only.
func TestPlaceNew_RemembersAnUnsupportedExclusiveRename(t *testing.T) {
	if !haveRenameNoReplace {
		t.Skip("no exclusive rename on this OS: the link path is the only one")
	}
	dir := t.TempDir()
	probes := 0
	pl := &placer{excl: func(src, dst string) error {
		probes++
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.ENOTSUP}
	}}
	for _, name := range []string{"a", "b", "c"} {
		src, dst := placeCase(t, dir, name)
		if err := pl.placeNew(src, dst); err != nil {
			t.Fatalf("%s: placeNew = %v", name, err)
		}
		if got := string(mustRead(t, dst)); got != "incoming "+name {
			t.Errorf("%s = %q", name, got)
		}
	}
	if probes != 1 || !pl.noExcl {
		t.Errorf("the unsupported call was tried %d times (noExcl=%v), want once", probes, pl.noExcl)
	}
	src, dst := placeCase(t, dir, "taken")
	writeFile(t, dst, []byte("theirs"))
	if err := pl.placeNew(src, dst); !errors.Is(err, errTargetAppeared) {
		t.Errorf("fallback onto an existing name = %v, want errTargetAppeared", err)
	}

	// A failure that says nothing about support (here: permission) is not
	// remembered: the next file tries the one-call rename again.
	probes = 0
	pl = &placer{excl: func(src, dst string) error {
		probes++
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EACCES}
	}}
	for _, name := range []string{"d", "e"} {
		src, dst := placeCase(t, dir, name)
		if err := pl.placeNew(src, dst); err != nil {
			t.Fatalf("%s: placeNew = %v", name, err)
		}
	}
	if probes != 2 || pl.noExcl {
		t.Errorf("probes = %d, noExcl = %v; want 2 and false", probes, pl.noExcl)
	}

	// An unsupported answer for a name the fallback can't place either
	// (the parent folder is missing) proves nothing about the filesystem.
	pl = &placer{excl: func(src, dst string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dst, Err: syscall.EINVAL}
	}}
	src, _ = placeCase(t, dir, "f")
	if err := pl.placeNew(src, filepath.Join(dir, "missing", "f")); err == nil || pl.noExcl {
		t.Errorf("placeNew into a missing folder = %v, noExcl = %v; want an error and false", err, pl.noExcl)
	}
}

// The real call on this OS answers "exists" for an existing name — the one
// answer placeNew must never mistake for a failure worth a fallback.
func TestRenameNoReplace_ExistingName(t *testing.T) {
	if !haveRenameNoReplace {
		t.Skip("no exclusive rename on this OS")
	}
	dir := t.TempDir()
	src, dst := placeCase(t, dir, "x")
	writeFile(t, dst, []byte("theirs"))
	err := renameNoReplace(src, dst)
	if renameNoReplaceUnsupported(err) {
		t.Skipf("the temp filesystem has no exclusive rename (%v)", err)
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("renameNoReplace onto an existing file = %v, want an exists error", err)
	}
	if got := string(mustRead(t, dst)); got != "theirs" {
		t.Errorf("the existing file was replaced: %q", got)
	}
	if err := os.Remove(dst); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplace(src, dst); err != nil {
		t.Fatalf("renameNoReplace onto an absent name = %v", err)
	}
	if got := string(mustRead(t, dst)); got != "incoming x" {
		t.Errorf("target = %q", got)
	}
}
