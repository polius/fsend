//go:build !windows

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// startReceiver launches `fsend <args>` in dir with its own stderr buffer.
func startReceiver(t *testing.T, xdg, dir string, stdin *os.File, args ...string) (*exec.Cmd, *safeBuffer, chan error) {
	t.Helper()
	cmd := h.fsendCmd(xdg, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	stderr := &safeBuffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start receiver: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd, stderr, done
}

// waitExit returns the receiver's exit code and how long it took after
// since, failing the test past budget.
func waitExit(t *testing.T, done chan error, since time.Time, budget time.Duration, stderr *safeBuffer) (int, time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		return exitCodeOrZero(err), time.Since(since)
	case <-time.After(budget):
		t.Fatalf("receiver still running %v later\n--- receiver stderr ---\n%s", budget, stderr.String())
		return -1, 0
	}
}

// A sender's Ctrl-C mid-transfer reaches the receiver at once as E032 —
// not as E020 after ~20 s of retrying a sender that is gone.
func TestPairing_SenderCancelMidTransferIsE032(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "big.bin")
	writeRandom(t, srcFile, 64*1024*1024)

	xdg := h.newXDG(t)
	s := h.startSender(t, xdg, srcFile)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	_, rErr, done := startReceiver(t, h.newXDG(t), dst, nil, "--yes", code)
	if !waitForSidecar(dst, 10*time.Second) {
		t.Fatalf("receiver never started writing\n%s", rErr.String())
	}
	s.signal(t, syscall.SIGINT)
	sent := time.Now()
	exit, took := waitExit(t, done, sent, 15*time.Second, rErr)
	if exit != 32 {
		t.Fatalf("receiver exit = %d, want 32 (E032)\n--- receiver stderr ---\n%s", exit, rErr.String())
	}
	if took > 5*time.Second {
		t.Errorf("receiver took %v to report the cancel", took)
	}
	if !strings.Contains(rErr.String(), "E032") {
		t.Errorf("no E032 in receiver output\n%s", rErr.String())
	}
}

// A receiver parked at the accept prompt learns of the sender's Ctrl-C
// instead of hanging on the prompt.
func TestPairing_SenderCancelAtPromptIsE032(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024)

	xdg := h.newXDG(t)
	s := h.startSender(t, xdg, srcFile)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	_, rErr, done := startReceiver(t, h.newXDG(t), dst, pr, code)
	if !waitForStderr(s, "Receiver connected", 10*time.Second) {
		t.Fatalf("receiver never paired\n%s", s.stderr.String())
	}
	time.Sleep(300 * time.Millisecond) // let the prompt render
	s.signal(t, syscall.SIGINT)
	exit, took := waitExit(t, done, time.Now(), 15*time.Second, rErr)
	if exit != 32 {
		t.Fatalf("receiver exit = %d, want 32 (E032)\n--- receiver stderr ---\n%s", exit, rErr.String())
	}
	if took > 5*time.Second {
		t.Errorf("receiver took %v to leave the prompt", took)
	}
}

// A second receiver on a code that is already in use is told so at once
// (E003), not "code not found" (E002) after a long wait.
func TestPairing_LateSecondReceiverGetsE003(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024)

	xdg := h.newXDG(t)
	s := h.startSender(t, xdg, srcFile)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	// Receiver 1 pairs (over LAN on this host) and parks at the prompt.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pr.Close(); _ = pw.Close() }()
	startReceiver(t, h.newXDG(t), dst, pr, code)
	if !waitForStderr(s, "Receiver connected", 10*time.Second) {
		t.Fatalf("receiver never paired\n%s", s.stderr.String())
	}
	time.Sleep(500 * time.Millisecond) // the losing server session is tombstoned in the background

	start := time.Now()
	_, rErr, exit := h.runFsendIn(t, h.newXDG(t), t.TempDir(), "--yes", code)
	took := time.Since(start)
	if exit != 3 {
		t.Fatalf("late receiver exit = %d, want 3 (E003)\n%s", exit, rErr)
	}
	if took > 5*time.Second {
		t.Errorf("late receiver took %v to hear E003", took)
	}
}

// A mistyped code fails fast with wording that says no sender is there.
func TestPairing_UnknownCodeFailsFast(t *testing.T) {
	requireE2E(t)
	start := time.Now()
	_, stderr, exit := h.runFsendIn(t, h.newXDG(t), t.TempDir(), "--yes", "zzz-zzzz-zzz")
	took := time.Since(start)
	if exit != 2 {
		t.Fatalf("exit = %d, want 2 (E002)\n%s", exit, stderr)
	}
	if took > 10*time.Second {
		t.Errorf("unknown code took %v to fail", took)
	}
	if !strings.Contains(stderr, "No sender is waiting on that code") {
		t.Errorf("unclear E002 wording:\n%s", stderr)
	}
}

// After a drop, only the original receiver device can re-pair: someone
// who read the code over a shoulder gets E003, while a rerun of the same
// command on the original device resumes.
func TestPairing_DroppedTransferNeedsOriginalReceiver(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024*1024)

	s := h.startSender(t, h.newXDG(t), srcFile)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	// The original receiver starts the transfer, then quits mid-stream.
	recvXDG := h.newXDG(t)
	r1, r1Err, r1Done := startReceiver(t, recvXDG, dst, nil, "--yes", code)
	if !waitForSidecarSize(dst, 1024*1024, 10*time.Second) {
		t.Fatalf("receiver never got going\n%s", r1Err.String())
	}
	if err := r1.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	<-r1Done
	if !waitForStderr(s, "Receiver disconnected", 30*time.Second) {
		t.Fatalf("sender never re-entered pairing\n%s", s.stderr.String())
	}

	// An onlooker with the code but not the session.
	_, oErr, exit := h.runFsendIn(t, h.newXDG(t), t.TempDir(), "--yes", code)
	if exit != 3 {
		t.Fatalf("onlooker exit = %d, want 3 (E003)\n%s", exit, oErr)
	}

	// The original device resumes with the same command.
	var r2Err bytes.Buffer
	r2 := h.fsendCmd(recvXDG, "--yes", code)
	r2.Dir = dst
	r2.Stderr = &r2Err
	if err := r2.Run(); err != nil {
		t.Fatalf("original receiver rerun failed (exit %d)\n%s\n--- sender ---\n%s",
			exitCodeOf(t, err), r2Err.String(), s.stderr.String())
	}
	if got := s.wait(t, 10*time.Second); got != 0 {
		t.Fatalf("sender exit = %d\n%s", got, s.stderr.String())
	}
	assertFilesEqual(t, srcFile, filepath.Join(dst, "p.bin"))
}
