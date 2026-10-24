package transfer

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/wire"
)

// seedPartial plants a partial for rel in dst's partial store, recorded as
// written from the source file at srcPath (its size and mtime).
func seedPartial(t *testing.T, dst, rel, srcPath string, data []byte) string {
	t.Helper()
	st, err := os.Stat(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	ps := newPartialStore(dst)
	f, path, err := ps.create(rel, wire.ListingEntry{Size: uint64(st.Size()), ModTimeSec: st.ModTime().Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	return path
}

func TestEngine_ResumeLargeFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(2*wire.MaxChunkSize + 4096)
	sp := filepath.Join(src, "big.bin")
	writeFile(t, sp, data)

	// Pre-seed a chunk-aligned partial with the first 2 chunks.
	partial := seedPartial(t, dst, "big.bin", sp, data[:2*wire.MaxChunkSize])

	var resumedAt uint64
	resumed := false
	var doneSHA string
	se, re := fileTransfer(t, []string{sp}, dst, func(o *RecvOptions) {
		o.OnResume = func(_ uint32, off, _ uint64) { resumed = true; resumedAt = off }
		o.OnFileDone = func(_, sha string) { doneSHA = sha }
		o.OnManifest = func([]ManifestEntry) {}
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if !resumed || resumedAt != 2*wire.MaxChunkSize {
		t.Errorf("expected resume at %d, got resumed=%v at %d", 2*wire.MaxChunkSize, resumed, resumedAt)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "big.bin")), data) {
		t.Fatal("resumed file content mismatch")
	}
	// The reported SHA-256 must cover the assembled file — resumed prefix
	// plus the tail that crossed the wire — not just the received bytes.
	if want := sha256hex(data); doneSHA != want {
		t.Errorf("resumed file SHA-256 = %q, want %q", doneSHA, want)
	}
	if _, err := os.Stat(partial); err == nil {
		t.Error("partial still present after commit")
	}
	if _, err := os.Stat(PartialDir(dst)); err == nil {
		t.Error("empty partial dir left behind")
	}
}

// A partial whose bytes differ from the source anywhere in the prefix (not
// just at imohash sample points) restarts the file from 0: the transfer
// succeeds instead of failing as "corrupted".
func TestEngine_ResumePartialMismatchRestarts(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(2*wire.MaxChunkSize + 10)
	sp := filepath.Join(src, "big.bin")
	writeFile(t, sp, data)

	stale := append([]byte(nil), data[:2*wire.MaxChunkSize]...)
	stale[200_000] ^= 0x01 // one flipped byte, between imohash's old samples
	seedPartial(t, dst, "big.bin", sp, stale)

	resumed := false
	se, re := fileTransfer(t, []string{sp}, dst, func(o *RecvOptions) {
		o.OnResume = func(uint32, uint64, uint64) { resumed = true }
	})
	if se != nil || re != nil {
		t.Fatalf("mismatched partial must restart, not fail: send=%v recv=%v", se, re)
	}
	if resumed {
		t.Error("a mismatched prefix was resumed")
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "big.bin")), data) {
		t.Fatal("restarted file content mismatch")
	}
}

// A leftover partial from a different version of the file (other size or
// mtime, per its sidecar) is ignored and the file is sent fresh.
func TestEngine_StalePartialFromOtherSourceIgnored(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(2*wire.MaxChunkSize + 10)
	sp := filepath.Join(src, "video.mp4")
	writeFile(t, sp, data)
	old := filepath.Join(t.TempDir(), "video.mp4")
	writeFile(t, old, randBytes(3*wire.MaxChunkSize))
	seedPartial(t, dst, "video.mp4", old, randBytes(2*wire.MaxChunkSize))

	resumed := false
	se, re := fileTransfer(t, []string{sp}, dst, func(o *RecvOptions) {
		o.OnResume = func(uint32, uint64, uint64) { resumed = true }
	})
	if se != nil || re != nil || resumed {
		t.Fatalf("send=%v recv=%v resumed=%v", se, re, resumed)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "video.mp4")), data) {
		t.Fatal("content mismatch")
	}
}

// A stale partial next to an existing, differing target must not let resume
// rename over the user's file without consent. Without --overwrite the file
// is kept (conflict); with it, the fresh content lands.
func TestEngine_ResumeDoesNotClobberExistingTarget(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(3 * wire.MaxChunkSize)
	writeFile(t, filepath.Join(src, "report.pdf"), data)

	// A leftover partial from a crashed transfer...
	seedPartial(t, dst, "report.pdf", filepath.Join(src, "report.pdf"), data[:2*wire.MaxChunkSize])
	// ...and the user's own, unrelated file at the same name.
	userFile := []byte("MY OWN FILE")
	writeFile(t, filepath.Join(dst, "report.pdf"), userFile)

	// No overwrite: the user's file is kept and the conflict is signalled
	// (the engine skips it; the E013 exit is the CLI layer's job).
	var kept []string
	_, re := fileTransfer(t, []string{filepath.Join(src, "report.pdf")}, dst, func(o *RecvOptions) {
		o.OnConflictKept = func(rel string) { kept = append(kept, rel) }
	})
	if re != nil {
		t.Fatalf("recv = %v, want nil (conflict kept, not an error)", re)
	}
	if len(kept) != 1 {
		t.Fatalf("expected 1 kept conflict, got %v", kept)
	}
	if got := mustRead(t, filepath.Join(dst, "report.pdf")); !bytes.Equal(got, userFile) {
		t.Fatalf("user's file was clobbered: %q", got)
	}

	// With overwrite: the transferred content lands (fresh, not resumed junk).
	_, re = fileTransfer(t, []string{filepath.Join(src, "report.pdf")}, dst, func(o *RecvOptions) {
		o.Overwrite = true
	})
	if re != nil {
		t.Fatalf("overwrite recv = %v", re)
	}
	if got := mustRead(t, filepath.Join(dst, "report.pdf")); !bytes.Equal(got, data) {
		t.Fatal("overwrite did not land the sent content")
	}
}

func TestEngine_StreamToSink(t *testing.T) {
	data := randBytes(3*wire.MaxChunkSize + 123)
	var sink bytes.Buffer
	se, re := runTransfer(t,
		SendOptions{Mode: wire.ModeStream, Stream: bytes.NewReader(data), DisplayName: "stream"},
		RecvOptions{TargetDir: t.TempDir(), Sink: &sink},
	)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if !bytes.Equal(sink.Bytes(), data) {
		t.Fatal("sink content mismatch")
	}
}

// A zero-byte file to a sink (--out -) must complete promptly: the sender
// skips the data phase for empty files, so the receiver must not block waiting
// for a chunk that never comes (it used to hang until the idle timeout).
func TestEngine_EmptyFileToSink(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "empty.bin"), nil)
	sources, err := Walk([]string{filepath.Join(src, "empty.bin")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	done := make(chan [2]error, 1)
	go func() {
		se, re := runTransfer(t, SendOptions{Mode: wire.ModeFiles, Sources: sources},
			RecvOptions{TargetDir: t.TempDir(), Sink: &sink})
		done <- [2]error{se, re}
	}()
	select {
	case errs := <-done:
		if errs[0] != nil || errs[1] != nil {
			t.Fatalf("send=%v recv=%v", errs[0], errs[1])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("empty file to sink hung (regression: deadlock waiting for a data chunk)")
	}
	if sink.Len() != 0 {
		t.Errorf("empty file should yield no bytes, got %d", sink.Len())
	}
}

func TestEngine_StreamToFile(t *testing.T) {
	dst := t.TempDir()
	data := []byte("wifi: hunter2")
	var donePath, doneSHA string
	se, re := runTransfer(t,
		SendOptions{Mode: wire.ModeStream, IsText: true, Stream: bytes.NewReader(data), DisplayName: "msg.txt"},
		RecvOptions{TargetDir: dst, OnFileDone: func(p, sha string) { donePath, doneSHA = p, sha }},
	)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if donePath == "" || !bytes.Equal(mustRead(t, donePath), data) {
		t.Fatalf("stream file mismatch at %q", donePath)
	}
	if want := sha256hex(data); doneSHA != want {
		t.Fatalf("stream SHA-256 = %q, want %q", doneSHA, want)
	}
}

// --out - with a folder streams a tar archive: every file and folder in
// listing order, with content, mode and mtime.
func TestEngine_FolderToSinkIsTar(t *testing.T) {
	src := t.TempDir()
	root := filepath.Join(src, "proj")
	files := map[string][]byte{
		"proj/a.txt":     []byte("alpha"),
		"proj/sub/b.bin": randBytes(wire.MaxChunkSize + 77),
		"proj/sub/e.txt": nil,
	}
	for rel, b := range files {
		writeFile(t, filepath.Join(src, rel), b)
	}
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	sources, err := Walk([]string{root}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sink bytes.Buffer
	se, re := runTransfer(t, SendOptions{Mode: wire.ModeFiles, Sources: sources},
		RecvOptions{TargetDir: t.TempDir(), Sink: &sink})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	tr := tar.NewReader(&sink)
	got := map[string][]byte{}
	var dirs []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeDir {
			dirs = append(dirs, h.Name)
			continue
		}
		b, _ := io.ReadAll(tr)
		got[h.Name] = b
	}
	for rel, want := range files {
		if !bytes.Equal(got[rel], want) {
			t.Errorf("%s: got %d bytes, want %d", rel, len(got[rel]), len(want))
		}
	}
	if strings.Join(dirs, ",") != "proj/,proj/empty/,proj/sub/" {
		t.Errorf("dirs = %v", dirs)
	}
}

func TestValidateListing_RejectsOutOfOrder(t *testing.T) {
	ooo := []wire.ListingEntry{{Index: 5, RelativePath: "a", Type: wire.EntryFile}}
	if err := validateListing(ooo); err == nil {
		t.Error("expected out-of-order index rejection")
	}
}

// A skipped (identical) file must never be re-written from the data stream:
// a transfer where everything is identical opens no files on disk.
func TestEngine_SkippedFileNeverReopened(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(wire.MaxChunkSize + 50)
	writeFile(t, filepath.Join(src, "a.bin"), data)

	// First transfer lands it (and sets mtime so the re-send classifies identical).
	if se, re := fileTransfer(t, []string{filepath.Join(src, "a.bin")}, dst, nil); se != nil || re != nil {
		t.Fatalf("seed send=%v recv=%v", se, re)
	}
	// Mark the on-disk copy so any spurious rewrite is detectable, preserving
	// size+mtime so it still classifies identical.
	target := filepath.Join(dst, "a.bin")
	st, _ := os.Stat(target)
	marker := append([]byte(nil), data...)
	marker[0] ^= 0xFF
	if err := os.WriteFile(target, marker, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(target, st.ModTime(), st.ModTime())

	se, re := fileTransfer(t, []string{filepath.Join(src, "a.bin")}, dst, nil)
	if se != nil || re != nil {
		t.Fatalf("resend send=%v recv=%v", se, re)
	}
	// The skip must not have touched the local (marked) bytes.
	if got := mustRead(t, target); got[0] != marker[0] {
		t.Error("a skipped file's local bytes were overwritten from the stream")
	}
}

// A source file that grows between the listing walk and the send must not
// abort the transfer: the sender ships a clean snapshot of the declared size,
// and the receiver gets exactly those bytes.
func TestEngine_SourceGrowsDuringTransfer(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	sp := filepath.Join(src, "log.txt")
	original := randBytes(wire.MaxChunkSize + 500)
	writeFile(t, sp, original)

	// Walk captures the size now...
	sources, err := Walk([]string{sp}, nil)
	if err != nil {
		t.Fatal(err)
	}
	// ...then the file grows on disk before we stream it.
	f, err := os.OpenFile(sp, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write(randBytes(wire.MaxChunkSize))
	_ = f.Close()

	se, re := runTransfer(t, SendOptions{Mode: wire.ModeFiles, Sources: sources}, RecvOptions{TargetDir: dst})
	if se != nil || re != nil {
		t.Fatalf("a growing source must not abort: send=%v recv=%v", se, re)
	}
	got := mustRead(t, filepath.Join(dst, "log.txt"))
	if !bytes.Equal(got, original) {
		t.Fatalf("receiver got %d bytes, want the %d-byte snapshot", len(got), len(original))
	}
}

func TestClassify_RejectsExactDuplicate(t *testing.T) {
	entries := []wire.ListingEntry{
		{Index: 0, RelativePath: "a/b.txt", Type: wire.EntryFile},
		{Index: 1, RelativePath: "a/./b.txt", Type: wire.EntryFile},
	}
	if _, err := classify(entries, t.TempDir(), false); err == nil {
		t.Error("expected duplicate rejection after cleaning")
	}
}

func TestClassify_RejectsPathTraversal(t *testing.T) {
	entries := []wire.ListingEntry{{Index: 0, RelativePath: "../escape.txt", Type: wire.EntryFile}}
	if _, err := classify(entries, t.TempDir(), false); err == nil {
		t.Error("expected path-traversal rejection")
	}
}
