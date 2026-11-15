package transfer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// dirNames lists dir's entries, comma-separated.
func dirNames(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return strings.Join(names, ",")
}

// Keep-both under --checksum: a differing file an earlier run kept as
// "f (1).txt" is not copied again, whether the local edit kept the size (the
// case that gained a copy on every run) or changed it, from --rename and
// from the prompt, and even after the copy's time drifted. The kept copy is
// recognised by content, never by size and time.
func TestChecksum_KeepBothDoesNotDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name, mine string
		viaPrompt  bool
	}{
		{"same-size edit, --rename", "MINE!!", false},
		{"same-size edit, prompt", "MINE!!", true},
		{"other-size edit, --rename", "mine, edited", false},
		{"other-size edit, prompt", "mine, edited", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			sp := filepath.Join(src, "f.txt")
			writeFile(t, sp, []byte("theirs"))
			writeFile(t, filepath.Join(dst, "f.txt"), []byte(tc.mine))
			run := func() (keptAs string, skipped int) {
				se, re := checksumTransfer(t, []string{sp}, dst, func(o *RecvOptions) {
					o.Accept = func(_ wire.SenderHello, s ClassifySummary) bool {
						if len(s.Files) == 1 {
							keptAs = s.Files[0].KeptAs
						}
						return true
					}
					o.OnSkip = func(uint32) { skipped++ }
					if tc.viaPrompt {
						o.ResolveConflicts = func([]Conflict) ConflictPolicy { return ConflictRename }
					} else {
						o.Conflict = ConflictRename
					}
				})
				if se != nil || re != nil {
					t.Fatalf("send=%v recv=%v", se, re)
				}
				return keptAs, skipped
			}
			if keptAs, _ := run(); keptAs != "" {
				t.Fatalf("first run already reports a kept copy %q", keptAs)
			}
			if got := dirNames(t, dst); got != "f (1).txt,f.txt" {
				t.Fatalf("after the first run: %s", got)
			}
			for i := range 2 {
				if i == 1 {
					// Another tool touched the copy: its time no longer matches.
					drift := time.Unix(1_600_000_000, 0)
					if err := os.Chtimes(filepath.Join(dst, "f (1).txt"), drift, drift); err != nil {
						t.Fatal(err)
					}
				}
				keptAs, skipped := run()
				if keptAs != "f (1).txt" || skipped != 1 {
					t.Errorf("run %d: keptAs=%q skipped=%d, want f (1).txt counted up to date", i+2, keptAs, skipped)
				}
				if got := dirNames(t, dst); got != "f (1).txt,f.txt" {
					t.Errorf("run %d left %s, want the one kept copy", i+2, got)
				}
			}
			if b := mustRead(t, filepath.Join(dst, "f.txt")); string(b) != tc.mine {
				t.Errorf("the user's file changed: %q", b)
			}
			if b := mustRead(t, filepath.Join(dst, "f (1).txt")); string(b) != "theirs" {
				t.Errorf("the kept copy changed: %q", b)
			}
		})
	}
}

// The other direction: a numbered copy with the incoming file's size and
// time but other content is not the incoming version. The default check
// takes it for one; --checksum must not, and saves the real one beside it.
func TestChecksum_KeepBothIgnoresALookalikeCopy(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	sp := filepath.Join(src, "f.txt")
	writeFile(t, sp, []byte("theirs"))
	writeFile(t, filepath.Join(dst, "f.txt"), []byte("MINE!!"))
	writeFile(t, filepath.Join(dst, "f (1).txt"), []byte("decoy!")) // same size as "theirs"
	st, err := os.Stat(sp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dst, "f (1).txt"), st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	se, re := checksumTransfer(t, []string{sp}, dst, func(o *RecvOptions) { o.Conflict = ConflictRename })
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := dirNames(t, dst); got != "f (1).txt,f (2).txt,f.txt" {
		t.Fatalf("files = %s, want the incoming version saved as f (2).txt", got)
	}
	if b := mustRead(t, filepath.Join(dst, "f (2).txt")); string(b) != "theirs" {
		t.Errorf("f (2).txt = %q", b)
	}
	if b := mustRead(t, filepath.Join(dst, "f (1).txt")); string(b) != "decoy!" {
		t.Errorf("the lookalike changed: %q", b)
	}
	// And the next run finds the real copy behind the lookalike.
	skipped := 0
	se, re = checksumTransfer(t, []string{sp}, dst, func(o *RecvOptions) {
		o.Conflict = ConflictRename
		o.OnSkip = func(uint32) { skipped++ }
	})
	if se != nil || re != nil || skipped != 1 {
		t.Fatalf("second run: send=%v recv=%v skipped=%d", se, re, skipped)
	}
	if got := dirNames(t, dst); got != "f (1).txt,f (2).txt,f.txt" {
		t.Errorf("second run left %s", got)
	}
}
