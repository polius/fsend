package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWrite_AtomicReplace checks the write is atomic and leaves no tempfiles:
// a second write replaces the first, the requested permissions are applied,
// and the tempfile is renamed (not left behind).
func TestWrite_AtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "target")

	if err := Write(path, []byte("one"), 0o600); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := Write(path, []byte("two"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "two" {
		t.Errorf("content = %q, want %q", b, "two")
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Errorf("perm = %o, want 600", got)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "target" {
		t.Errorf("dir has %d entries %v, want just the target (no tempfile leftovers)", len(entries), entries)
	}
}
