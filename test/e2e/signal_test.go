//go:build !windows

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
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

	// Poll for the sidecar instead of a fixed sleep — a 400 ms wait was
	// below CI's noise floor on fast Ubuntu runners, where the 64 MiB
	// loopback transfer could finish before SIGINT landed and the
	// receiver would legitimately exit 0. Sidecar presence proves the
	// receiver is actively writing chunks, so SIGINT is guaranteed to
	// interrupt an in-flight transfer.
	if !waitForSidecar(dst, 5*time.Second) {
		_ = s.cmd.Process.Kill()
		_ = rCmd.Process.Kill()
		<-rDone
		t.Fatal("no .fsend-partial sidecar appeared within 5s — receiver never started writing")
	}
	s.signal(t, syscall.SIGINT)
	s.wait(t, 5*time.Second)

	// Generous bound: the receiver may walk its full retry budget
	// (3 × 10s QUIC handshake) when SIGINT lands at certain stream
	// positions before giving up. We only care that it eventually
	// surfaces a non-zero exit; the duration is bounded by retry config.
	select {
	case err := <-rDone:
		if err == nil {
			t.Fatalf("receiver returned 0 after sender SIGINT — should be non-zero")
		}
	case <-time.After(30 * time.Second):
		_ = rCmd.Process.Kill()
		<-rDone
		t.Fatalf("receiver did not exit within 30s")
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
// coverage goal.
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

	// Poll for the sidecar instead of sleeping. Its existence proves
	// the receiver has gone past the QUIC handshake and is writing
	// chunks — i.e. the SIGINT that follows will actually interrupt a
	// transfer in flight, which is the property under test. A fixed
	// sleep (the old 400 ms) was below CI's noise floor and produced
	// flakes on loaded runners.
	if !waitForSidecar(dst, 5*time.Second) {
		_ = s.cmd.Process.Kill()
		_ = r1.Process.Kill()
		_ = r1.Wait()
		t.Fatal("no .fsend-partial sidecar appeared within 5s — receiver never started writing")
	}

	s.signal(t, syscall.SIGINT)
	s.wait(t, 5*time.Second)
	_ = r1.Process.Kill()
	_ = r1.Wait()

	// Brief settle so the LAN port is fully released.
	time.Sleep(250 * time.Millisecond)

	// Attempt 2 — same dst dir; resume should kick in.
	r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "big.bin"))
}

// waitForSidecar polls for any *.fsend-partial under dir, returning
// true once one appears or false on timeout. 25 ms cadence is short
// enough to catch a transfer that bursts past in <100 ms without
// busy-spinning.
func waitForSidecar(dir string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.fsend-partial"))
		if len(matches) > 0 {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
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

// The flagship recovery path: the receiver dies mid-transfer (Ctrl-C)
// and is re-run with the same code. The sender must re-enter pairing —
// fresh mDNS announce, fresh server session — and the rerun must resume
// from the .fsend-partial rather than restart.
func TestSignal_ReceiverSIGINTRerunSameCode(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "big.bin")
	writeRandom(t, srcFile, 64*1024*1024)

	xdg := h.newXDG(t)
	s := h.startSender(t, xdg, srcFile)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	// Attempt 1 — interrupt the receiver once a meaningful prefix is on
	// disk, so the rerun has something to resume from.
	r1 := h.fsendCmd(xdg, "--yes", code)
	r1.Dir = dst
	var r1Err bytes.Buffer
	r1.Stderr = &r1Err
	if err := r1.Start(); err != nil {
		t.Fatalf("start recv1: %v", err)
	}
	if !waitForSidecarSize(dst, 4*1024*1024, 10*time.Second) {
		_ = r1.Process.Kill()
		_ = r1.Wait()
		t.Fatal("sidecar never reached 4 MiB — receiver never got going")
	}
	if err := r1.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signal recv1: %v", err)
	}
	_ = r1.Wait()

	// The interrupt must explain itself: the force-quit escape hatch and
	// the fact that the partial survives for the resume exercised below.
	if !strings.Contains(r1Err.String(), "press Ctrl-C again to force quit") {
		t.Errorf("no force-quit hint on first SIGINT\n--- recv1 stderr ---\n%s", r1Err.String())
	}
	if !strings.Contains(r1Err.String(), "Partial data kept") {
		t.Errorf("no partial-kept resume hint after mid-transfer SIGINT\n--- recv1 stderr ---\n%s", r1Err.String())
	}

	// The sender must go back to waiting rather than exit.
	if !waitForStderr(s, "Receiver disconnected", 30*time.Second) {
		t.Fatalf("sender never re-entered pairing\n--- sender stderr ---\n%s", s.stderr.String())
	}

	// Attempt 2 — same code, same dst: must pair again and resume.
	r2 := h.fsendCmd(xdg, "--yes", code)
	r2.Dir = dst
	var r2Err bytes.Buffer
	r2.Stderr = &r2Err
	if err := r2.Run(); err != nil {
		t.Fatalf("rerun receiver failed (exit %d)\n--- recv2 stderr ---\n%s\n--- sender stderr ---\n%s",
			exitCodeOf(t, err), r2Err.String(), s.stderr.String())
	}
	if got := s.wait(t, 10*time.Second); got != 0 {
		t.Fatalf("sender exit = %d\n--- sender stderr ---\n%s", got, s.stderr.String())
	}
	if !strings.Contains(r2Err.String(), "Resuming from") {
		t.Errorf("rerun did not resume from the partial\n--- recv2 stderr ---\n%s", r2Err.String())
	}
	assertFilesEqual(t, srcFile, filepath.Join(dst, "big.bin"))
}

// waitForSidecarSize polls until a *.fsend-partial under dir reaches
// minBytes, or the budget elapses.
func waitForSidecarSize(dir string, minBytes int64, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		matches, _ := filepath.Glob(filepath.Join(dir, "*.fsend-partial"))
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && st.Size() >= minBytes {
				return true
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// waitForStderr polls a sender's stderr for substr within budget.
func waitForStderr(s *senderProc, substr string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if strings.Contains(s.stderr.String(), substr) {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return false
}

// A receiver that dies uncleanly (SIGKILL — crash, power loss, network
// gone) must not strand the sender: after the QUIC idle timeout and the
// bounded re-accept retries, the sender reports the lost contact
// honestly (no "reconnect" from a dead process) and returns to waiting —
// and the same code must still complete a transfer with a fresh
// receiver. FSEND_QUIC_IDLE_TIMEOUT shrinks the 30 s death-detection
// window so the test doesn't wait a minute per run.
func TestSignal_ReceiverKilledCodeStaysLive(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 256*1024)

	xdg := h.newXDG(t)
	s := &senderProc{cmd: h.fsendCmd(xdg, srcFile), stdout: &safeBuffer{}, stderr: &safeBuffer{}}
	s.cmd.Env = append(s.cmd.Env, "FSEND_QUIC_IDLE_TIMEOUT=2s")
	s.cmd.Stdout, s.cmd.Stderr = s.stdout, s.stderr
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("start sender: %v", err)
	}
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	// Receiver 1 pairs (no --yes: it parks at the accept prompt, held
	// open by a pipe we never write to) and is then hard-killed. An
	// os.Pipe passes the fd straight through, so Wait doesn't hang on
	// an exec stdin-copy goroutine after the Kill.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	r1 := h.fsendCmd(xdg, code)
	r1.Dir = dst
	r1.Stdin = pr
	var r1Err bytes.Buffer
	r1.Stderr = &r1Err
	if err := r1.Start(); err != nil {
		t.Fatalf("start recv1: %v", err)
	}
	if !waitForStderr(s, "Receiver connected", 10*time.Second) {
		t.Fatalf("receiver never paired\n--- sender stderr ---\n%s", s.stderr.String())
	}
	if err := r1.Process.Kill(); err != nil {
		t.Fatalf("kill recv1: %v", err)
	}
	_ = r1.Wait()

	// Idle timeout (2 s) + two bounded re-accepts (15 s each) + backoff.
	if !waitForStderr(s, "Lost contact with the receiver", 60*time.Second) {
		t.Fatalf("sender never reported the lost receiver\n--- sender stderr ---\n%s", s.stderr.String())
	}
	// The user-facing retry notice must carry a rounded wait, not the raw
	// jittered duration ("618.167744ms").
	if rawDuration.MatchString(s.stderr.String()) {
		t.Errorf("retry notice leaked a raw duration\n--- sender stderr ---\n%s", s.stderr.String())
	}

	// The code is still live: a fresh receiver completes the transfer.
	rOut, rErr, exit := h.runFsendIn(t, xdg, dst, "--yes", code)
	if exit != 0 {
		t.Fatalf("fresh receiver exit %d\n%s\n%s\n--- sender stderr ---\n%s", exit, rOut, rErr, s.stderr.String())
	}
	if got := s.wait(t, 10*time.Second); got != 0 {
		t.Fatalf("sender exit = %d\n--- sender stderr ---\n%s", got, s.stderr.String())
	}
	assertFilesEqual(t, srcFile, filepath.Join(dst, "p.bin"))
}

// rawDuration matches Go's un-rounded Duration rendering (5+ decimals),
// e.g. "618.167744ms" — what the retry notice must never show.
var rawDuration = regexp.MustCompile(`[0-9]+\.[0-9]{5,}m?s`)
