package transfer

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/polius/fsend/internal/wire"
)

func checksumTransfer(t *testing.T, srcPaths []string, dstDir string, mutate func(*RecvOptions)) (error, error) {
	t.Helper()
	sources, err := Walk(srcPaths, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	o := RecvOptions{TargetDir: dstDir, Checksum: true}
	if mutate != nil {
		mutate(&o)
	}
	return runTransfer(t, SendOptions{Mode: wire.ModeFiles, Sources: sources}, o)
}

// --checksum skips identical content even when the modification times differ
// (the case the default stat check would needlessly re-send).
func TestChecksum_SkipsIdenticalDespiteMtimeDrift(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(wire.MaxChunkSize + 17)
	writeFile(t, filepath.Join(src, "f.bin"), data)
	writeFile(t, filepath.Join(dst, "f.bin"), data) // identical content...
	// ...but a deliberately different mtime (as if placed by another tool).
	old := time.Unix(1_600_000_000, 0)
	if err := os.Chtimes(filepath.Join(dst, "f.bin"), old, old); err != nil {
		t.Fatal(err)
	}

	skipped := 0
	se, re := checksumTransfer(t, []string{filepath.Join(src, "f.bin")}, dst, func(o *RecvOptions) {
		o.OnSkip = func(uint32) { skipped++ }
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if skipped != 1 {
		t.Fatalf("--checksum should skip identical content despite mtime drift; skipped=%d", skipped)
	}
}

// The false-skip the user flagged: same path, same size, same mtime, but
// DIFFERENT content. The default would wrongly skip; --checksum must catch it
// and treat the file as differing (kept without --overwrite).
func TestChecksum_CatchesSameSizeSameMtimeDifferentContent(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	a := []byte("AAAAAAAAAAAA") // 12 bytes
	b := []byte("BBBBBBBBBBBB") // 12 bytes, same size, different content
	writeFile(t, filepath.Join(src, "f.bin"), a)
	writeFile(t, filepath.Join(dst, "f.bin"), b)
	// Force identical mtime so the default stat check could not tell them apart.
	ts := time.Unix(1_700_000_000, 0)
	_ = os.Chtimes(filepath.Join(src, "f.bin"), ts, ts)
	_ = os.Chtimes(filepath.Join(dst, "f.bin"), ts, ts)

	// Sanity: the default (no checksum) is fooled and skips — proving the hole
	// --checksum exists to close.
	if se, re := fileTransfer(t, []string{filepath.Join(src, "f.bin")}, dst, nil); se != nil || re != nil {
		t.Fatalf("default send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "f.bin")); !bytes.Equal(got, b) {
		t.Fatalf("default unexpectedly changed the file")
	}

	// With --checksum: the file is recognized as differing and kept (no
	// --overwrite), and the receiver reports the conflict.
	kept := 0
	se, re := checksumTransfer(t, []string{filepath.Join(src, "f.bin")}, dst, func(o *RecvOptions) {
		o.OnConflictKept = func(string) { kept++ }
	})
	if se != nil || re != nil {
		t.Fatalf("checksum send=%v recv=%v", se, re)
	}
	if kept != 1 {
		t.Fatalf("--checksum should detect the content difference; kept=%d", kept)
	}
	if got := mustRead(t, filepath.Join(dst, "f.bin")); !bytes.Equal(got, b) {
		t.Error("local file clobbered without --overwrite")
	}

	// And with --checksum --overwrite, it gets replaced.
	se, re = checksumTransfer(t, []string{filepath.Join(src, "f.bin")}, dst, func(o *RecvOptions) {
		o.Overwrite = true
	})
	if se != nil || re != nil {
		t.Fatalf("checksum+overwrite send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "f.bin")); !bytes.Equal(got, a) {
		t.Error("--checksum --overwrite should have replaced the differing file")
	}
}

// --checksum on a fresh destination still transfers everything (no candidates
// to verify, no verify round-trip stalls).
func TestChecksum_FreshDestination(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), []byte("one"))
	writeFile(t, filepath.Join(src, "b.txt"), randBytes(2*wire.MaxChunkSize))
	se, re := checksumTransfer(t, []string{filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")}, dst, nil)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "a.txt")), []byte("one")) {
		t.Error("a.txt mismatch")
	}
}
