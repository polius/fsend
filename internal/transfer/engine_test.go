package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/retry"
	"github.com/polius/fsend/internal/wire"
)

// harness wires Send and Recv over in-memory full-duplex pipes and runs them
// concurrently, returning both errors.
func runTransfer(t *testing.T, sendOpts SendOptions, recvOpts RecvOptions) (sendErr, recvErr error) {
	t.Helper()
	ctrlA, ctrlB := net.Pipe()
	dataA, dataB := net.Pipe()
	sender := &Streams{Control: ctrlA, Data: dataA}
	receiver := &Streams{Control: ctrlB, Data: dataB}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); sendErr = Send(ctx, sender, sendOpts); sender.Close() }()
	go func() { defer wg.Done(); recvErr = Recv(ctx, receiver, recvOpts); receiver.Close() }()
	wg.Wait()
	return sendErr, recvErr
}

// fileTransfer sends srcDir's contents into dstDir and returns both errors.
func fileTransfer(t *testing.T, srcPaths []string, dstDir string, mutate func(*RecvOptions)) (error, error) {
	t.Helper()
	sources, err := Walk(srcPaths, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	recvOpts := RecvOptions{TargetDir: dstDir}
	if mutate != nil {
		mutate(&recvOpts)
	}
	return runTransfer(t, SendOptions{Mode: wire.ModeFiles, Sources: sources}, recvOpts)
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func randBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestEngine_SingleFile(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	data := randBytes(3 * wire.MaxChunkSize)
	writeFile(t, filepath.Join(src, "report.pdf"), data)

	se, re := fileTransfer(t, []string{filepath.Join(src, "report.pdf")}, dst, nil)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "report.pdf")), data) {
		t.Fatal("content mismatch")
	}
}

// The whole stat-based default rests on fsend setting the source's mtime on
// the receiver, so a re-send matches. Prove both: the date is preserved to the
// second, and a second send then classifies as identical (skipped) — with no
// hashing, on stat alone.
func TestEngine_PreservesModTimeEnablingSkip(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	sp := filepath.Join(src, "f.bin")
	writeFile(t, sp, randBytes(wire.MaxChunkSize+9))
	// Stamp a distinct, sub-second-bearing mtime on the source.
	want := time.Unix(1_700_000_123, 456_000_000)
	if err := os.Chtimes(sp, want, want); err != nil {
		t.Fatal(err)
	}

	if se, re := fileTransfer(t, []string{sp}, dst, nil); se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}

	// Receiver's mtime must equal the source's, truncated to the second.
	dstSt, err := os.Stat(filepath.Join(dst, "f.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if dstSt.ModTime().Unix() != want.Unix() {
		t.Fatalf("receiver mtime = %d, want %d (source second-granularity)", dstSt.ModTime().Unix(), want.Unix())
	}

	// Re-send: must be skipped purely on size+mtime (no content hashing).
	var skipped int
	se, re := fileTransfer(t, []string{sp}, dst, func(o *RecvOptions) {
		o.OnSkip = func(uint32) { skipped++ }
	})
	if se != nil || re != nil {
		t.Fatalf("resend send=%v recv=%v", se, re)
	}
	if skipped != 1 {
		t.Fatalf("re-send should skip on stat alone; skipped=%d", skipped)
	}
}

func TestEngine_Directory(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	files := map[string][]byte{
		"proj/a.txt":         []byte("hello"),
		"proj/sub/b.bin":     randBytes(2*wire.MaxChunkSize + 7),
		"proj/sub/deep/c.go": []byte("package main"),
		"proj/empty.txt":     {},
	}
	for rel, data := range files {
		writeFile(t, filepath.Join(src, rel), data)
	}
	if err := os.MkdirAll(filepath.Join(src, "proj/emptydir"), 0o755); err != nil {
		t.Fatal(err)
	}

	se, re := fileTransfer(t, []string{filepath.Join(src, "proj")}, dst, nil)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	for rel, data := range files {
		if !bytes.Equal(mustRead(t, filepath.Join(dst, rel)), data) {
			t.Errorf("%s content mismatch", rel)
		}
	}
	if st, err := os.Stat(filepath.Join(dst, "proj/emptydir")); err != nil || !st.IsDir() {
		t.Error("empty dir not recreated")
	}
}

// `fsend .` sends the current directory's *contents*, not a wrapper folder:
// the receiver gets the files at the top level, no parent-folder name.
func TestEngine_DotSendsContentsNotWrapper(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "a.txt"), []byte("alpha"))
	writeFile(t, filepath.Join(src, "sub", "b.txt"), []byte("beta"))

	// Walk(".") with cwd = src must root entries at their own names, never at
	// src's basename.
	t.Chdir(src)
	sources, err := Walk([]string{"."}, nil)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Base(src)
	for _, s := range sources {
		if s.Entry.RelativePath == base || strings.HasPrefix(s.Entry.RelativePath, base+"/") {
			t.Fatalf("`.` wrapped contents under %q: %q", base, s.Entry.RelativePath)
		}
	}

	// End-to-end: the receiver gets a.txt and sub/b.txt directly.
	se, re := runTransfer(t, SendOptions{Mode: wire.ModeFiles, Sources: sources}, RecvOptions{TargetDir: dst})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "a.txt")), []byte("alpha")) {
		t.Error("a.txt should land at the top level")
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "sub", "b.txt")), []byte("beta")) {
		t.Error("sub/b.txt should land at the top level")
	}
	if _, err := os.Stat(filepath.Join(dst, base)); err == nil {
		t.Errorf("no wrapper folder %q should exist on the receiver", base)
	}
}

func TestIsContentsRef(t *testing.T) {
	for _, c := range []struct {
		p    string
		want bool
	}{
		{".", true}, {"./", true}, {"foo/.", true}, {"./.", true},
		{"foo", false}, {"foo/", false}, {"..", false}, {"foo/bar", false},
	} {
		if got := IsContentsRef(c.p); got != c.want {
			t.Errorf("IsContentsRef(%q) = %v, want %v", c.p, got, c.want)
		}
	}
}

func TestEngine_ManyTinyFilesPacked(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	want := map[string][]byte{}
	for i := 0; i < 500; i++ {
		rel := filepath.Join("tiny", "f"+pad(i))
		data := []byte("content-" + pad(i))
		want[rel] = data
		writeFile(t, filepath.Join(src, rel), data)
	}
	se, re := fileTransfer(t, []string{filepath.Join(src, "tiny")}, dst, nil)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	for rel, data := range want {
		if !bytes.Equal(mustRead(t, filepath.Join(dst, rel)), data) {
			t.Errorf("%s mismatch", rel)
		}
	}
}

// More files than a single chunk's uint16 segment count can hold must be
// split across chunks: the packer flushes on the segment cap, not only when
// the byte buffer fills. Exercised at the packer level (no disk/pipe) so it
// stays fast on every platform.
func TestPacker_FlushesOnSegmentCap(t *testing.T) {
	var buf bytes.Buffer
	p, err := newChunkPacker(&buf)
	if err != nil {
		t.Fatal(err)
	}
	const n = maxSegmentsPerChunk + 50 // crosses the cap once → ≥2 chunks
	for i := 0; i < n; i++ {
		d := []byte{byte(i), byte(i >> 8)} // tiny files; byte buffer never fills
		if err := p.appendBytes(uint32(i), d); err != nil {
			t.Fatal(err)
		}
		if err := p.endFile(uint32(i), blakeHash32(d)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}
	p.Close()

	chunks := 0
	finalized := make(map[uint32]bool)
	for {
		c, err := wire.ReadChunk(&buf)
		if err != nil {
			break // drained
		}
		chunks++
		if len(c.Segments) > maxSegmentsPerChunk {
			t.Fatalf("chunk %d has %d segments, over the %d cap", chunks, len(c.Segments), maxSegmentsPerChunk)
		}
		for _, s := range c.Segments {
			if s.EOF {
				finalized[s.FileIndex] = true
			}
		}
	}
	if chunks < 2 {
		t.Fatalf("segment cap should force ≥2 chunks, got %d", chunks)
	}
	if len(finalized) != n {
		t.Fatalf("finalized %d files, want %d", len(finalized), n)
	}
}

func TestEngine_SkipIdenticalOnResend(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	a := randBytes(wire.MaxChunkSize + 100)
	b := []byte("second file")
	writeFile(t, filepath.Join(src, "a.bin"), a)
	writeFile(t, filepath.Join(src, "b.txt"), b)

	// First transfer.
	se, re := fileTransfer(t, []string{filepath.Join(src, "a.bin"), filepath.Join(src, "b.txt")}, dst, nil)
	if se != nil || re != nil {
		t.Fatalf("first send=%v recv=%v", se, re)
	}

	// Second transfer: everything identical → all skipped, nothing sent.
	var skipped []uint32
	se, re = fileTransfer(t, []string{filepath.Join(src, "a.bin"), filepath.Join(src, "b.txt")}, dst, func(o *RecvOptions) {
		o.OnSkip = func(i uint32) { skipped = append(skipped, i) }
	})
	if se != nil || re != nil {
		t.Fatalf("second send=%v recv=%v", se, re)
	}
	if len(skipped) != 2 {
		t.Errorf("expected 2 skipped, got %d", len(skipped))
	}
}

// A permission-only change (chmod +x) leaves content, size and mtime
// untouched, so the file still classifies identical — but the new mode must
// still land on the receiver's copy instead of being silently lost.
func TestEngine_PermissionChangePropagatesOnIdenticalResend(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	sp := filepath.Join(src, "deploy.sh")
	writeFile(t, sp, []byte("#!/bin/sh\necho hi\n"))
	if err := os.Chmod(sp, 0o644); err != nil {
		t.Fatal(err)
	}

	// First transfer lands it non-executable.
	if se, re := fileTransfer(t, []string{sp}, dst, nil); se != nil || re != nil {
		t.Fatalf("seed send=%v recv=%v", se, re)
	}
	dp := filepath.Join(dst, "deploy.sh")
	if st, _ := os.Stat(dp); st.Mode()&0o111 != 0 {
		t.Fatal("precondition: dst should start non-executable")
	}

	// chmod +x on the source (content/size/mtime unchanged), re-send.
	st, _ := os.Stat(sp)
	if err := os.Chmod(sp, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chtimes(sp, st.ModTime(), st.ModTime())

	var skipped int
	if se, re := fileTransfer(t, []string{sp}, dst, func(o *RecvOptions) {
		o.OnSkip = func(uint32) { skipped++ }
	}); se != nil || re != nil {
		t.Fatalf("resend send=%v recv=%v", se, re)
	}
	if skipped != 1 {
		t.Errorf("expected the file to skip the data transfer, got skipped=%d", skipped)
	}
	if st, _ := os.Stat(dp); st.Mode()&os.ModePerm != 0o755 {
		t.Errorf("mode not propagated: dst is %o, want 0755", st.Mode()&os.ModePerm)
	}
}

func TestEngine_DifferingFileProtectedWithoutOverwrite(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "x.txt"), []byte("NEW CONTENT"))
	writeFile(t, filepath.Join(dst, "x.txt"), []byte("old local edit"))

	var kept []string
	se, re := fileTransfer(t, []string{filepath.Join(src, "x.txt")}, dst, func(o *RecvOptions) {
		o.OnConflictKept = func(rel string) { kept = append(kept, rel) }
		// no Overwrite, no ConfirmOverwrite → keep local
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "x.txt")); string(got) != "old local edit" {
		t.Errorf("local file was clobbered: %q", got)
	}
	if len(kept) != 1 {
		t.Errorf("expected 1 conflict kept, got %d", len(kept))
	}
}

func TestEngine_DifferingFileOverwritten(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "x.txt"), []byte("NEW CONTENT"))
	writeFile(t, filepath.Join(dst, "x.txt"), []byte("old"))

	se, re := fileTransfer(t, []string{filepath.Join(src, "x.txt")}, dst, func(o *RecvOptions) {
		o.Overwrite = true
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if got := mustRead(t, filepath.Join(dst, "x.txt")); string(got) != "NEW CONTENT" {
		t.Errorf("overwrite failed: %q", got)
	}
}

func TestEngine_TypeConflictFileVsDir(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	// Source: foo is a directory. Dest: foo is a file.
	writeFile(t, filepath.Join(src, "foo", "inner.txt"), []byte("inner"))
	writeFile(t, filepath.Join(dst, "foo"), []byte("i am a file"))

	// Without overwrite → kept (the file survives, dir not created).
	se, re := fileTransfer(t, []string{filepath.Join(src, "foo")}, dst, nil)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if st, err := os.Stat(filepath.Join(dst, "foo")); err != nil || st.IsDir() {
		t.Error("conflict: existing file should be kept without --overwrite")
	}

	// With overwrite → the file is replaced by the directory tree.
	se, re = fileTransfer(t, []string{filepath.Join(src, "foo")}, dst, func(o *RecvOptions) { o.Overwrite = true })
	if se != nil || re != nil {
		t.Fatalf("overwrite send=%v recv=%v", se, re)
	}
	if !bytes.Equal(mustRead(t, filepath.Join(dst, "foo", "inner.txt")), []byte("inner")) {
		t.Error("conflict: dir tree should replace the file with --overwrite")
	}
}

func TestEngine_Manifest(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "new.txt"), []byte("new"))
	writeFile(t, filepath.Join(src, "same.txt"), []byte("same"))
	writeFile(t, filepath.Join(dst, "same.txt"), []byte("same"))
	// Make mtime match so the identical one is skipped.
	srcSt, _ := os.Stat(filepath.Join(src, "same.txt"))
	_ = os.Chtimes(filepath.Join(dst, "same.txt"), srcSt.ModTime(), srcSt.ModTime())

	var got []ManifestEntry
	// A real receive — OnManifest fires after it completes, recording what
	// fsend did with each file.
	se, re := fileTransfer(t, []string{filepath.Join(src, "new.txt"), filepath.Join(src, "same.txt")}, dst, func(o *RecvOptions) {
		o.OnManifest = func(e []ManifestEntry) { got = e }
	})
	if se != nil || re != nil {
		t.Fatalf("transfer send=%v recv=%v", se, re)
	}
	status := map[string]string{}
	for _, e := range got {
		status[e.RelativePath] = e.Status
	}
	if status["new.txt"] != "new" {
		t.Errorf("new.txt status = %q, want new", status["new.txt"])
	}
	if status["same.txt"] != "identical" {
		t.Errorf("same.txt status = %q, want identical", status["same.txt"])
	}
	// It's a real receive: the new file actually landed.
	if _, err := os.Stat(filepath.Join(dst, "new.txt")); err != nil {
		t.Errorf("new.txt should have been written: %v", err)
	}
}

func TestEngine_Decline(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "f.txt"), []byte("data"))
	se, re := fileTransfer(t, []string{filepath.Join(src, "f.txt")}, dst, func(o *RecvOptions) {
		o.Accept = func(wire.SenderHello, ClassifySummary) bool { return false }
	})
	if re == nil || se == nil {
		t.Fatalf("expected decline errors, got send=%v recv=%v", se, re)
	}
	if _, err := os.Stat(filepath.Join(dst, "f.txt")); err == nil {
		t.Error("declined transfer still wrote a file")
	}
}

func TestEngine_SummaryBreakdown(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(src, "new.txt"), []byte("n"))
	writeFile(t, filepath.Join(src, "same.txt"), []byte("s"))
	writeFile(t, filepath.Join(dst, "same.txt"), []byte("s"))
	srcSt, _ := os.Stat(filepath.Join(src, "same.txt"))
	_ = os.Chtimes(filepath.Join(dst, "same.txt"), srcSt.ModTime(), srcSt.ModTime())

	var summary ClassifySummary
	se, re := fileTransfer(t, []string{filepath.Join(src, "new.txt"), filepath.Join(src, "same.txt")}, dst, func(o *RecvOptions) {
		o.Accept = func(_ wire.SenderHello, s ClassifySummary) bool { summary = s; return true }
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	if summary.NewItems != 1 || summary.Identical != 1 {
		t.Errorf("summary = %+v, want 1 new / 1 identical", summary)
	}
}

func pad(i int) string { return fmt.Sprintf("%03d", i) }

// manifestStatus is the single source of truth for the --manifest "status"
// column, so pin every (disposition, decision) combination it can see.
// TestEngine_Manifest only exercises new/identical end-to-end; the
// overwritten/kept/resumed branches are unreachable without a prior
// local copy and an overwrite choice, so cover them here directly.
func TestManifestStatus(t *testing.T) {
	for _, tc := range []struct {
		disp disposition
		act  wire.DecisionAction
		want string
	}{
		{dispNew, wire.DecisionSend, "new"},
		{dispDiffers, wire.DecisionSend, "overwritten"},
		{dispConflict, wire.DecisionSend, "overwritten"},
		{dispIdentical, wire.DecisionSkip, "identical"},
		{dispDiffers, wire.DecisionSkip, "kept"},
		{dispConflict, wire.DecisionSkip, "kept"},
		{dispResume, wire.DecisionResume, "resumed"},
	} {
		if got := manifestStatus(tc.disp, tc.act); got != tc.want {
			t.Errorf("manifestStatus(%v, %v) = %q, want %q", tc.disp, tc.act, got, tc.want)
		}
	}
}

// A sender whose source vanishes between walk and send must tell the
// receiver why. Without the ERROR frame the receiver saw only a bare
// stream close, classified it transient, and burned retries on a
// misleading "network" error.
func TestEngine_SenderReadFailureReachesReceiver(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	p := filepath.Join(src, "gone.bin")
	writeFile(t, p, randBytes(1024))
	sources, err := Walk([]string{p}, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	se, re := runTransfer(t,
		SendOptions{Mode: wire.ModeFiles, Sources: sources},
		RecvOptions{TargetDir: dst},
	)
	if !errors.Is(se, fserrors.ErrReadFailed) {
		t.Fatalf("send: want ErrReadFailed, got %v", se)
	}
	if !errors.Is(re, fserrors.ErrReadFailed) {
		t.Fatalf("recv: want the sender's ErrReadFailed, got %v", re)
	}
	if retry.IsTransient(re) {
		t.Fatalf("receiver error must be terminal (no retry), got transient: %v", re)
	}
}

// passwordTransfer runs a password-gated single-file transfer with the given
// receiver password options.
func passwordTransfer(t *testing.T, senderPass string, mutate func(*RecvOptions)) (dst string, sendErr, recvErr error) {
	t.Helper()
	src := t.TempDir()
	dst = t.TempDir()
	writeFile(t, filepath.Join(src, "gated.txt"), []byte("payload"))
	sources, err := Walk([]string{filepath.Join(src, "gated.txt")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	recvOpts := RecvOptions{TargetDir: dst}
	mutate(&recvOpts)
	sendErr, recvErr = runTransfer(t,
		SendOptions{Mode: wire.ModeFiles, Sources: sources, Password: senderPass}, recvOpts)
	return dst, sendErr, recvErr
}

// A typo at the prompt gets fresh challenges: two wrong tries then the right
// one completes the transfer on the same session, without burning the code.
func TestEngine_PasswordRetriesWithinCap(t *testing.T) {
	tries := []string{"wrong1", "wrong2", "right"}
	var attempts []int
	dst, se, re := passwordTransfer(t, "right", func(o *RecvOptions) {
		o.PromptPass = func(attempt int) (string, error) {
			attempts = append(attempts, attempt)
			return tries[attempt-1], nil
		}
	})
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v", se, re)
	}
	want := []int{1, 2, 3}
	if len(attempts) != 3 || attempts[0] != want[0] || attempts[1] != want[1] || attempts[2] != want[2] {
		t.Errorf("prompt attempts = %v, want %v", attempts, want)
	}
	if got := mustRead(t, filepath.Join(dst, "gated.txt")); string(got) != "payload" {
		t.Errorf("file content = %q", got)
	}
}

// PasswordAttempts wrong tries abort with ErrWrongPassword on both sides.
func TestEngine_PasswordExhaustedAborts(t *testing.T) {
	prompts := 0
	dst, se, re := passwordTransfer(t, "right", func(o *RecvOptions) {
		o.PromptPass = func(attempt int) (string, error) {
			prompts++
			return "wrong", nil
		}
	})
	if !errors.Is(se, fserrors.ErrWrongPassword) || !errors.Is(re, fserrors.ErrWrongPassword) {
		t.Fatalf("send=%v recv=%v, want ErrWrongPassword on both", se, re)
	}
	if prompts != PasswordAttempts {
		t.Errorf("prompted %d times, want %d", prompts, PasswordAttempts)
	}
	if _, err := os.Stat(filepath.Join(dst, "gated.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("no file must land on a failed password gate")
	}
}

// A fixed password (--password / FSEND_PASSWORD) can't change between tries,
// so a mismatch aborts after one attempt on both sides.
func TestEngine_PasswordFixedWrongSingleAttempt(t *testing.T) {
	_, se, re := passwordTransfer(t, "right", func(o *RecvOptions) {
		o.Password = "wrong"
	})
	if !errors.Is(se, fserrors.ErrWrongPassword) || !errors.Is(re, fserrors.ErrWrongPassword) {
		t.Fatalf("send=%v recv=%v, want ErrWrongPassword on both", se, re)
	}
}
