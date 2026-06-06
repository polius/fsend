//go:build !windows

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Sender SIGINT mid-transfer must leave the receiver in a clean
// non-zero state with no full-size destination file.
func TestSignal_SenderSIGINTMidTransfer(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "big.bin")
	const size = 64 * 1024 * 1024
	writeRandom(t, srcFile, size)

	xdg := h.newXDG(t)
	s := h.startSender(t, xdg, srcFile)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	// Launch the receiver in a goroutine; interrupt the sender mid-flow.
	rCmd := h.fsendCmd(xdg, "--yes", code)
	rCmd.Dir = dst
	var rOut, rErr bytes.Buffer
	rCmd.Stdout = &rOut
	rCmd.Stderr = &rErr
	if err := rCmd.Start(); err != nil {
		t.Fatalf("start receiver: %v", err)
	}
	rDone := make(chan error, 1)
	go func() { rDone <- rCmd.Wait() }()

	time.Sleep(400 * time.Millisecond)
	s.signal(t, syscall.SIGINT)
	s.wait(t, 5*time.Second)

	select {
	case err := <-rDone:
		if err == nil {
			t.Fatalf("receiver returned 0 after sender SIGINT — should be non-zero")
		}
	case <-time.After(5 * time.Second):
		_ = rCmd.Process.Kill()
		<-rDone
		t.Fatalf("receiver did not exit within 5s")
	}

	if fi, err := os.Stat(filepath.Join(dst, "big.bin")); err == nil {
		if fi.Size() == int64(size) {
			t.Fatalf("dst file is full size — should have been interrupted")
		}
	}
}

// SIGINT before any receiver pairs must let the sender shut down within
// a few seconds (signalContext + LAN listener teardown).
func TestSignal_SenderSIGINTPreTransfer(t *testing.T) {
	requireE2E(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSender(t, h.newXDG(t), filepath.Join(src, "x"))
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })

	_ = s.waitForCode(t, 5*time.Second)
	s.signal(t, syscall.SIGINT)
	if exit := s.wait(t, 3*time.Second); exit == -1 {
		t.Fatal("sender did not exit within 3s of SIGINT")
	}
}

// Three sequential transfers exercise socket and port reuse on both
// sides.
func TestSignal_SequentialTransfers(t *testing.T) {
	requireE2E(t)
	for i := 0; i < 3; i++ {
		src, dst := t.TempDir(), t.TempDir()
		srcFile := filepath.Join(src, "p.bin")
		writeRandom(t, srcFile, 256*1024)
		r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
		r.requireSuccess(t)
		assertFilesEqual(t, srcFile, filepath.Join(dst, "p.bin"))
	}
}

// Sending a nonexistent path must exit non-zero without crashing.
func TestSignal_NonexistentSource(t *testing.T) {
	requireE2E(t)
	_, _, code := h.runFsend(t, h.newXDG(t),
		"--send", "--quiet", filepath.Join(t.TempDir(), "does-not-exist.bin"))
	if code == 0 {
		t.Fatal("expected non-zero exit on missing source")
	}
}

// After a successful transfer the .fsend-partial sidecar must not be
// left behind.
func TestSignal_NoPartialAfterSuccess(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 256*1024)
	r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)

	matches, _ := filepath.Glob(filepath.Join(dst, "*.fsend-partial"))
	if len(matches) > 0 {
		t.Fatalf("sidecar(s) left behind: %v", matches)
	}
}

// Resume across an interrupted attempt: send → SIGINT mid-stream →
// re-send with same dst → final file is byte-identical.
//
// Receiver1 is killed once the sender is gone instead of waited on —
// its retry budget (3 attempts × 10s QUIC handshake) would otherwise
// stretch this test to ~25s, and its clean exit is not part of the
// coverage goal. The partial sidecar that resume relies on has already
// been written by the time chunks have flowed for 400ms.
func TestSignal_ResumeAfterSIGINT(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "big.bin")
	writeRandom(t, srcFile, 64*1024*1024)

	xdg := h.newXDG(t)

	// Attempt 1 — kill the sender mid-stream so a sidecar lands.
	s := h.startSender(t, xdg, srcFile)
	code1 := s.waitForCode(t, 5*time.Second)
	r1 := h.fsendCmd(xdg, "--yes", code1)
	r1.Dir = dst
	var r1Err bytes.Buffer
	r1.Stderr = &r1Err
	if err := r1.Start(); err != nil {
		t.Fatalf("start recv1: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	s.signal(t, syscall.SIGINT)
	s.wait(t, 5*time.Second)
	_ = r1.Process.Kill()
	_ = r1.Wait()

	// Confirm a partial sidecar landed; otherwise attempt 2 isn't a
	// real resume and the test would be silently degenerate.
	if matches, _ := filepath.Glob(filepath.Join(dst, "*.fsend-partial")); len(matches) == 0 {
		t.Fatal("no .fsend-partial sidecar after attempt 1 — resume coverage degenerate")
	}

	// Brief settle so the LAN port is fully released.
	time.Sleep(250 * time.Millisecond)

	// Attempt 2 — same dst dir; resume should kick in.
	r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "big.bin"))
}

// Plant a chunk-aligned partial sidecar and confirm the receiver elects
// ActionResume: the destination file's inode matches the planted
// partial, proving the bytes were not re-written from scratch.
func TestSignal_ResumeReusesPartialInodePreserved(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "big.bin")
	writeRandom(t, srcFile, 4*1024*1024)

	// Plant a 2 MiB partial = first half of the source.
	body, err := os.ReadFile(srcFile)
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(dst, "big.bin.fsend-partial")
	if err := os.WriteFile(partial, body[:2*1024*1024], 0o644); err != nil {
		t.Fatal(err)
	}

	inoBefore, err := inodeOf(partial)
	if err != nil {
		t.Fatalf("stat partial: %v", err)
	}

	r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)

	dstFile := filepath.Join(dst, "big.bin")
	assertFilesEqual(t, srcFile, dstFile)
	if _, err := os.Stat(partial); err == nil {
		t.Fatalf("sidecar left behind after resume")
	}

	inoAfter, err := inodeOf(dstFile)
	if err != nil {
		t.Fatalf("stat dst: %v", err)
	}
	if inoBefore != inoAfter {
		t.Fatalf("inode changed %d → %d — receiver re-created the file instead of resuming",
			inoBefore, inoAfter)
	}
}

func inodeOf(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, nil
	}
	return uint64(st.Ino), nil
}
