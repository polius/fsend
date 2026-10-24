package transfer

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/unicode/norm"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// Two names this filesystem treats as one (NFC/NFD, or case) must never
// silently overwrite each other: the first lands, the second is skipped and
// reported. Classified with explicit semantics so every rule runs on any host.
func TestClassify_UnicodeAndCaseCollisions(t *testing.T) {
	nfc, nfd := "café.txt", norm.NFD.String("café.txt")
	entries := []wire.ListingEntry{
		{Index: 0, RelativePath: nfc, Type: wire.EntryFile, Size: 1},
		{Index: 1, RelativePath: nfd, Type: wire.EntryFile, Size: 1},
		{Index: 2, RelativePath: "Été", Type: wire.EntryFile, Size: 1},
		{Index: 3, RelativePath: "été", Type: wire.EntryFile, Size: 1},
	}
	for _, tc := range []struct {
		sem     fsSemantics
		refused []uint32
	}{
		{fsSemantics{}, nil},                                          // ext4: all four distinct
		{fsSemantics{foldCase: true}, []uint32{3}},                    // NTFS: case only
		{fsSemantics{foldNorm: true}, []uint32{1}},                    // normalization only
		{fsSemantics{foldCase: true, foldNorm: true}, []uint32{1, 3}}, // APFS default
	} {
		dir := t.TempDir()
		c := &classifier{targetDir: dir, sem: tc.sem, ps: newPartialStore(dir)}
		plans, err := c.run(entries)
		if err != nil {
			t.Fatal(err)
		}
		var got []uint32
		for _, p := range plans {
			if p.disp == dispRefused {
				got = append(got, p.entry.Index)
				if !strings.Contains(p.skipReason, "same name as") {
					t.Errorf("reason = %q", p.skipReason)
				}
			}
		}
		if len(got) != len(tc.refused) || (len(got) > 0 && !equalU32(got, tc.refused)) {
			t.Errorf("%+v: refused %v, want %v", tc.sem, got, tc.refused)
		}
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// End to end on the real filesystem: a colliding pair reports one skip and
// keeps the first file's bytes intact (no "Saved 2 files" with one on disk).
func TestEngine_CaseCollisionOnThisFS(t *testing.T) {
	dst := t.TempDir()
	if !probeFS(dst).foldCase {
		t.Skip("case-sensitive filesystem: the pair doesn't collide here")
	}
	entries := []wire.ListingEntry{
		{Index: 0, RelativePath: "dir", Type: wire.EntryDir, Mode: 0o755},
		{Index: 1, RelativePath: "dir/Readme.md", Type: wire.EntryFile, Size: 5, Mode: 0o644},
		{Index: 2, RelativePath: "dir/README.md", Type: wire.EntryFile, Size: 5, Mode: 0o644},
	}
	var skipped []string
	sender, decisions, recvErr := hostileSetup(t, dst, entries, RecvOptions{
		OnEntrySkipped: func(rel, _ string) { skipped = append(skipped, rel) },
	})
	if decisions[2].Action != wire.DecisionSkip || decisions[1].Action != wire.DecisionSend {
		t.Fatalf("decisions = %+v", decisions)
	}
	go func() {
		_ = wire.WriteChunk(sender.Data, mkChunk(1, []byte("first"), true))
		_ = wire.WriteControl(sender.Control, wire.TypeTransferComplete, nil)
	}()
	if err := awaitReject(sender, recvErr); err != nil {
		t.Fatalf("recv = %v", err)
	}
	if len(skipped) != 1 || skipped[0] != "dir/README.md" {
		t.Fatalf("skipped = %v", skipped)
	}
	if got := mustRead(t, filepath.Join(dst, "dir", "Readme.md")); string(got) != "first" {
		t.Fatalf("first file = %q", got)
	}
}

// --overwrite must never delete a folder with contents to place a file.
func TestEngine_OverwriteNeverDeletesNonEmptyFolder(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "docs"), []byte("a file named docs"))
	writeFile(t, filepath.Join(dst, "docs", "thesis.tex"), []byte("years of work"))

	var skipped []string
	se, re := fileTransfer(t, []string{filepath.Join(src, "docs")}, dst, func(o *RecvOptions) {
		o.Overwrite = true
		o.OnEntrySkipped = func(rel, _ string) { skipped = append(skipped, rel) }
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "docs", "thesis.tex")); string(got) != "years of work" {
		t.Fatalf("folder contents lost: %q", got)
	}
	if len(skipped) != 1 {
		t.Fatalf("skipped = %v, want the incoming file reported", skipped)
	}

	// An empty folder in the way is fine to replace.
	_ = os.RemoveAll(filepath.Join(dst, "docs"))
	if err := os.Mkdir(filepath.Join(dst, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if se, re := fileTransfer(t, []string{filepath.Join(src, "docs")}, dst, func(o *RecvOptions) { o.Overwrite = true }); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "docs")); string(got) != "a file named docs" {
		t.Fatalf("docs = %q", got)
	}
}

// Keep both: the incoming copy lands as "name (1).ext", the local one stays,
// and a folder clash moves the whole incoming folder.
func TestEngine_KeepBothRenames(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "report.pdf"), []byte("incoming"))
	writeFile(t, filepath.Join(dst, "report.pdf"), []byte("local"))
	writeFile(t, filepath.Join(dst, "report (1).pdf"), []byte("older copy"))
	writeFile(t, filepath.Join(src, "proj", "main.go"), []byte("package main"))
	writeFile(t, filepath.Join(dst, "proj"), []byte("a file where the folder goes"))

	var asked []Conflict
	var manifest []ManifestEntry
	se, re := fileTransfer(t, []string{filepath.Join(src, "report.pdf"), filepath.Join(src, "proj")}, dst, func(o *RecvOptions) {
		o.ResolveConflicts = func(c []Conflict) ConflictPolicy { asked = c; return ConflictRename }
		o.OnManifest = func(m []ManifestEntry) { manifest = m }
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if len(asked) != 2 {
		t.Fatalf("asked about %d conflicts, want 2", len(asked))
	}
	for path, want := range map[string]string{
		"report.pdf":       "local",
		"report (1).pdf":   "older copy",
		"report (2).pdf":   "incoming",
		"proj":             "a file where the folder goes",
		"proj (1)/main.go": "package main",
	} {
		if got := mustRead(t, filepath.Join(dst, filepath.FromSlash(path))); string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	statuses := map[string]string{}
	for _, m := range manifest {
		statuses[m.RelativePath] = m.Status
	}
	if statuses["report (2).pdf"] != "renamed" || statuses["proj (1)/main.go"] != "renamed" {
		t.Errorf("manifest = %+v", manifest)
	}
}

// An approved replace of an empty file goes through a new inode, so a
// hardlink to the old file keeps its content.
func TestEngine_EmptyFileReplaceKeepsHardlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hardlink semantics differ")
	}
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "e.txt"), nil)
	writeFile(t, filepath.Join(dst, "e.txt"), []byte("linked content"))
	if err := os.Link(filepath.Join(dst, "e.txt"), filepath.Join(dst, "h.txt")); err != nil {
		t.Fatal(err)
	}
	if se, re := fileTransfer(t, []string{filepath.Join(src, "e.txt")}, dst, func(o *RecvOptions) { o.Overwrite = true }); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "e.txt")); len(got) != 0 {
		t.Fatalf("e.txt = %q, want empty", got)
	}
	if got := mustRead(t, filepath.Join(dst, "h.txt")); string(got) != "linked content" {
		t.Fatalf("hardlink truncated: %q", got)
	}
}

// Names of 242–255 bytes used to overflow once ".fsend-partial" was added.
func TestEngine_LongNameFits(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	name := strings.Repeat("n", 250) + ".bin"
	data := randBytes(wire.MaxChunkSize + 5)
	writeFile(t, filepath.Join(src, name), data)
	if se, re := fileTransfer(t, []string{filepath.Join(src, name)}, dst, nil); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, name)), data) {
		t.Fatal("content mismatch")
	}
}

// Zero runs arrive as holes but read back as the exact bytes, including a
// trailing hole that must still give the file its full size.
func TestEngine_SparseRunsRoundTrip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := make([]byte, 3*wire.MaxChunkSize+12345)
	copy(data[wire.MaxChunkSize:], randBytes(100_000))
	copy(data[2*wire.MaxChunkSize+7:], []byte("tail marker"))
	writeFile(t, filepath.Join(src, "disk.img"), data) // ends in a zero run
	if se, re := fileTransfer(t, []string{filepath.Join(src, "disk.img")}, dst, nil); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "disk.img")), data) {
		t.Fatal("sparse content mismatch")
	}
}

// mtime is preserved even at or before the epoch (the old "> 0" guard
// dropped it), and FAT-family 2 s granularity counts as identical.
func TestEngine_EpochMtimeAndCoarseClock(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	sp := filepath.Join(src, "old.txt")
	writeFile(t, sp, []byte("1970"))
	epoch := time.Unix(0, 0)
	if err := os.Chtimes(sp, epoch, epoch); err != nil {
		t.Fatal(err)
	}
	if se, re := fileTransfer(t, []string{sp}, dst, nil); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	st, err := os.Stat(filepath.Join(dst, "old.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if st.ModTime().Unix() != 0 {
		t.Errorf("mtime = %v, want the epoch", st.ModTime())
	}

	fat := fsSemantics{coarseMtime: true}
	if !fat.sameMtime(time.Unix(1000, 0), 1001) || !fat.sameMtime(time.Unix(1002, 0), 1000) || fat.sameMtime(time.Unix(1003, 0), 1000) {
		t.Error("coarse clock tolerance should be ±2 s")
	}
	if (fsSemantics{}).sameMtime(time.Unix(1001, 0), 1000) {
		t.Error("exact clock must not tolerate drift")
	}
}

// --checksum: a content match also takes the sender's mtime, so the next
// plain (stat-based) run sees the file as identical without hashing.
func TestChecksum_MatchAdoptsSenderMtime(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(4096)
	writeFile(t, filepath.Join(src, "f.bin"), data)
	writeFile(t, filepath.Join(dst, "f.bin"), data)
	_ = os.Chtimes(filepath.Join(dst, "f.bin"), time.Unix(1_600_000_000, 0), time.Unix(1_600_000_000, 0))
	var progress uint64
	if se, re := checksumTransfer(t, []string{filepath.Join(src, "f.bin")}, dst, func(o *RecvOptions) {
		o.OnHashProgress = func(done, _ uint64) { progress = done }
	}); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if progress != uint64(len(data)) {
		t.Errorf("hash progress = %d, want %d", progress, len(data))
	}
	skipped := 0
	if se, re := fileTransfer(t, []string{filepath.Join(src, "f.bin")}, dst, func(o *RecvOptions) {
		o.OnSkip = func(uint32) { skipped++ }
	}); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if skipped != 1 {
		t.Fatal("plain re-run after --checksum match should skip on stat alone")
	}
}

// A hostile sender can't hold several files open at once: a segment for a
// second file while the first is unfinished is a protocol error.
func TestHostile_InterleavedFilesRejected(t *testing.T) {
	dst := t.TempDir()
	entries := []wire.ListingEntry{
		{Index: 0, RelativePath: "a.bin", Size: 8, Type: wire.EntryFile},
		{Index: 1, RelativePath: "b.bin", Size: 8, Type: wire.EntryFile},
	}
	sender, _, recvErr := hostileSetup(t, dst, entries, RecvOptions{})
	go func() {
		_ = wire.WriteChunk(sender.Data, mkChunk(0, []byte("aaaa"), false))
		_ = wire.WriteChunk(sender.Data, mkChunk(1, []byte("bbbb"), false))
	}()
	if err := awaitReject(sender, recvErr); !errors.Is(err, fserrors.ErrProtocolError) {
		t.Fatalf("recv = %v, want ErrProtocolError", err)
	}
	sender.Close()
}

// Messages for the peer carry the relative path and the error class, never
// the receiver's absolute paths.
func TestPeerSafe_NoAbsolutePaths(t *testing.T) {
	_, err := os.Open("/Users/someone/secret/dir/file.txt")
	msg := peerSafe("open", "dir/file.txt", err)
	if strings.Contains(msg, "/Users/someone") || !strings.Contains(msg, "dir/file.txt") {
		t.Fatalf("peer message = %q", msg)
	}
}

// Not enough free space is caught before the prompt, with a reason for the
// sender, instead of filling the disk.
func TestReceiver_CheckSpaceDeclines(t *testing.T) {
	r := &receiver{s: &Streams{Control: nopRWC{}}, opts: RecvOptions{TargetDir: t.TempDir()}}
	free, ok := freeSpace(r.opts.TargetDir)
	if !ok {
		t.Skip("free space unknown on this platform")
	}
	if err := r.checkSpace(1); err != nil {
		t.Fatalf("1 byte: %v", err)
	}
	if err := r.checkSpace(free + 1<<40); !errors.Is(err, fserrors.ErrDiskFull) {
		t.Fatalf("err = %v, want ErrDiskFull", err)
	}
}
