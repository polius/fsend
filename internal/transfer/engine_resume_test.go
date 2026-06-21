package transfer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/wire"
)

func TestEngine_ResumeLargeFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(2*wire.MaxChunkSize + 4096)
	writeFile(t, filepath.Join(src, "big.bin"), data)

	// Pre-seed a chunk-aligned partial with the first 2 chunks.
	prefix := data[:2*wire.MaxChunkSize]
	writeFile(t, filepath.Join(dst, "big.bin"+partialSuffix), prefix)

	var resumedAt uint64
	resumed := false
	se, re := fileTransfer(t, []string{filepath.Join(src, "big.bin")}, dst, func(o *RecvOptions) {
		o.OnResume = func(_ uint32, off, _ uint64) { resumed = true; resumedAt = off }
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
}

func TestEngine_ResumePartialMismatch(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(2*wire.MaxChunkSize + 10)
	writeFile(t, filepath.Join(src, "big.bin"), data)

	// Partial whose prefix does NOT match the source → sender must reject.
	bad := bytes.Repeat([]byte{0xEE}, 2*wire.MaxChunkSize)
	writeFile(t, filepath.Join(dst, "big.bin"+partialSuffix), bad)

	se, re := fileTransfer(t, []string{filepath.Join(src, "big.bin")}, dst, nil)
	if se == nil || re == nil {
		t.Fatalf("expected partial-mismatch errors, got send=%v recv=%v", se, re)
	}
	// The unreconcilable partial is discarded so a re-run starts clean.
	if _, err := os.Stat(filepath.Join(dst, "big.bin"+partialSuffix)); err == nil {
		t.Error("mismatched partial should have been discarded")
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

func TestEngine_StreamToFile(t *testing.T) {
	dst := t.TempDir()
	data := []byte("wifi: hunter2")
	var donePath string
	se, re := runTransfer(t,
		SendOptions{Mode: wire.ModeStream, IsText: true, Stream: bytes.NewReader(data), DisplayName: "msg.txt"},
		RecvOptions{TargetDir: dst, OnFileDone: func(p string) { donePath = p }},
	)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if donePath == "" || !bytes.Equal(mustRead(t, donePath), data) {
		t.Fatalf("stream file mismatch at %q", donePath)
	}
}

func TestEngine_StreamSinkRejectsMultiFile(t *testing.T) {
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), []byte("a"))
	writeFile(t, filepath.Join(src, "b.txt"), []byte("b"))
	sources, _ := Walk([]string{filepath.Join(src, "a.txt"), filepath.Join(src, "b.txt")}, nil)
	var sink bytes.Buffer
	_, re := runTransfer(t,
		SendOptions{Mode: wire.ModeFiles, Sources: sources},
		RecvOptions{TargetDir: t.TempDir(), Sink: &sink},
	)
	if re == nil || !strings.Contains(re.Error(), "stdout") {
		t.Fatalf("expected sink-rejects-multifile error, got %v", re)
	}
}

func TestValidateListing_RejectsDuplicateAndOutOfOrder(t *testing.T) {
	dup := []wire.ListingEntry{
		{Index: 0, RelativePath: "a.txt", Type: wire.EntryFile},
		{Index: 1, RelativePath: "A.txt", Type: wire.EntryFile}, // case-collision
	}
	if err := validateListing(dup); err == nil {
		t.Error("expected case-collision rejection")
	}
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

func TestClassify_RejectsPathTraversal(t *testing.T) {
	entries := []wire.ListingEntry{{Index: 0, RelativePath: "../escape.txt", Type: wire.EntryFile}}
	if _, err := classify(entries, t.TempDir(), false); err == nil {
		t.Error("expected path-traversal rejection")
	}
}
