package e2e

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --------------------------------------------------------------------
// LAN happy paths
// --------------------------------------------------------------------

func TestLAN_TransferSizes(t *testing.T) {
	requireE2E(t)
	cases := []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"small_128KB", 128 * 1024},
		{"medium_4MB", 4 * 1024 * 1024},
		{"large_16MB", 16 * 1024 * 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src, dst := t.TempDir(), t.TempDir()
			payload := filepath.Join(src, "payload.bin")
			writeRandom(t, payload, tc.size)

			r := h.runPair(t,
				[]string{payload}, dst, []string{"--yes"}, "")
			r.requireSuccess(t)

			assertFilesEqual(t, payload, filepath.Join(dst, "payload.bin"))
		})
	}
}

func TestLAN_FilenameWithSpaces(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "file with spaces.txt")
	if err := os.WriteFile(srcFile, []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "file with spaces.txt"))
}

func TestLAN_UnicodeFilename(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "résumé-é.txt")
	if err := os.WriteFile(srcFile, []byte("unicode-bytes\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "résumé-é.txt"))
}

// --------------------------------------------------------------------
// Send modes
// --------------------------------------------------------------------

func TestSend_Directory(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcRoot := filepath.Join(src, "tree")
	if err := os.MkdirAll(filepath.Join(srcRoot, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "a.txt"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcRoot, "b.txt"), []byte("beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRandom(t, filepath.Join(srcRoot, "sub", "c.bin"), 128*1024)

	r := h.runPair(t, []string{srcRoot}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)

	for _, rel := range []string{"a.txt", "b.txt", "sub/c.bin"} {
		assertFilesEqual(t,
			filepath.Join(srcRoot, rel),
			filepath.Join(dst, "tree", rel),
		)
	}
}

func TestSend_MultipleFiles(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "one"), []byte("1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "two"), []byte("2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeRandom(t, filepath.Join(src, "three"), 32*1024)

	r := h.runPair(t,
		[]string{
			filepath.Join(src, "one"),
			filepath.Join(src, "two"),
			filepath.Join(src, "three"),
		},
		dst, []string{"--yes"}, "")
	r.requireSuccess(t)

	for _, name := range []string{"one", "two", "three"} {
		assertFilesEqual(t,
			filepath.Join(src, name),
			filepath.Join(dst, name),
		)
	}
}

func TestSend_Stdin(t *testing.T) {
	requireE2E(t)
	dst := t.TempDir()
	xdg := h.newXDG(t)

	s := h.startSenderStdin(t, xdg, "hello-stdin-12345\n", "-")
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	rOut, rErr, exit := h.runFsendIn(t, xdg, dst, "--yes", code)
	s.wait(t, 5*time.Second)
	if exit != 0 {
		t.Fatalf("receiver exit %d\n%s\n%s", exit, rOut, rErr)
	}
	matches, err := filepath.Glob(filepath.Join(dst, "fsend-stdin-*"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no fsend-stdin-* file in %s", dst)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "hello-stdin-12345") {
		t.Fatalf("payload mismatch: %q", body)
	}
}

// TestSend_StdinStreaming pipes a multi-MiB random payload from a slow
// producer (two halves with a pause in between) through fsend's stdin
// path. The streaming path must move the bytes without buffering the
// whole stream in memory and the receiver must end up with the exact
// bytes the producer wrote.
func TestSend_StdinStreaming(t *testing.T) {
	requireE2E(t)

	// 3 × MaxChunkSize keeps the test fast while still crossing several
	// chunk boundaries — enough to catch a regression where the sender
	// silently reverts to "read everything before sending."
	const payloadSize = 3 * 1024 * 1024
	payload := make([]byte, payloadSize)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}

	pr, pw := io.Pipe()
	// Producer: emit the payload in two halves with a small pause so the
	// sender is forced to process bytes before EOF arrives. A buffered
	// implementation would block here waiting for the second write.
	go func() {
		defer pw.Close()
		_, _ = pw.Write(payload[:payloadSize/2])
		time.Sleep(100 * time.Millisecond)
		_, _ = pw.Write(payload[payloadSize/2:])
	}()

	dst := t.TempDir()
	xdg := h.newXDG(t)

	s := h.startSenderStdinReader(t, xdg, pr, "-")
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	code := s.waitForCode(t, 5*time.Second)

	rOut, rErr, exit := h.runFsendIn(t, xdg, dst, "--yes", code)
	s.wait(t, 15*time.Second)
	if exit != 0 {
		t.Fatalf("receiver exit %d\n%s\n%s", exit, rOut, rErr)
	}

	matches, err := filepath.Glob(filepath.Join(dst, "fsend-stdin-*"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no fsend-stdin-* file in %s", dst)
	}
	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("streamed payload mismatch: %d bytes received, %d sent", len(got), len(payload))
	}
}

func TestSend_TextLiteral(t *testing.T) {
	requireE2E(t)
	dst := t.TempDir()
	r := h.runPair(t,
		[]string{"--text", "literal-9876"},
		dst, []string{"--yes"}, "")
	r.requireSuccess(t)

	matches, err := filepath.Glob(filepath.Join(dst, "fsend-text-*.txt"))
	if err != nil || len(matches) == 0 {
		t.Fatalf("no fsend-text-*.txt file in %s", dst)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "literal-9876") {
		t.Fatalf("payload mismatch: %q", body)
	}
}

// --------------------------------------------------------------------
// Receiver flags
// --------------------------------------------------------------------

func TestReceive_OutDir(t *testing.T) {
	requireE2E(t)
	src, parent := t.TempDir(), t.TempDir()
	target := filepath.Join(parent, "sub")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024)

	r := h.runPair(t, []string{srcFile}, parent,
		[]string{"--yes", "--out", target}, "")
	r.requireSuccess(t)

	if _, err := os.Stat(filepath.Join(target, "p.bin")); err != nil {
		t.Fatalf("--out target missing file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "p.bin")); err == nil {
		t.Fatalf("file leaked into parent (should be in --out dir)")
	}
}

func TestReceive_Overwrite(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024)
	if err := os.WriteFile(filepath.Join(dst, "p.bin"), []byte("PREEXISTING"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.runPair(t, []string{srcFile}, dst,
		[]string{"--yes", "--overwrite"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "p.bin"))
}

// Interactive: receiver answers "y" to the accept prompt and "y" again
// to the overwrite confirmation. File is clobbered, both sides exit 0.
func TestReceive_OverwriteConfirmedInteractively(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 16*1024)
	if err := os.WriteFile(filepath.Join(dst, "p.bin"), []byte("PREEXISTING"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Two y's: one for accept, one for overwrite.
	r := h.runPair(t, []string{srcFile}, dst, nil, "y\ny\n")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "p.bin"))
}

// Interactive: receiver accepts the transfer but declines the overwrite.
// Existing file is preserved; receiver sees E013, sender sees E013.
func TestReceive_OverwriteDeclinedInteractively(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 16*1024)
	if err := os.WriteFile(filepath.Join(dst, "p.bin"), []byte("PREEXISTING"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.runPair(t, []string{srcFile}, dst, nil, "y\nn\n")
	if r.receiverExitCode != 13 || r.senderExitCode != 13 {
		t.Errorf("exits: sender=%d receiver=%d, want 13/13\nsender stderr:\n%s\nreceiver stderr:\n%s",
			r.senderExitCode, r.receiverExitCode, r.senderErr, r.receiverErr)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "p.bin")); string(got) != "PREEXISTING" {
		t.Errorf("destination file was clobbered: %q", got)
	}
}

// Regression: when the receiver rejects with target-exists, the sender
// must see E013 — not E099 ("Application error 0x0"). The fix is the
// symmetric shutdown in recv.go around the ErrCodeTargetExists frame;
// without it, the receiver's deferred QUIC close races the frame and
// the sender sees a transport-level error. The pipe-based unit test
// TestOverwriteRefused can't reproduce the race because pipes don't
// have QUIC's application close.
func TestReceive_OverwriteRefused_SenderSeesE013(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024)
	if err := os.WriteFile(filepath.Join(dst, "p.bin"), []byte("PREEXISTING"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.runPair(t, []string{srcFile}, dst, []string{"--yes"}, "")
	if r.receiverExitCode != 13 {
		t.Errorf("receiver exit = %d, want 13 (E013)\nstderr:\n%s",
			r.receiverExitCode, r.receiverErr)
	}
	if r.senderExitCode != 13 {
		t.Errorf("sender exit = %d, want 13 (E013) — likely the recv-side TargetExists frame raced QUIC close\nstderr:\n%s",
			r.senderExitCode, r.senderErr)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "p.bin")); string(got) != "PREEXISTING" {
		t.Errorf("destination file was clobbered: %q", got)
	}
}

func TestReceive_NameSurfacesToPeer(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// No --yes: the receiver shows the interactive prompt where the
	// sender's --name override should appear.
	r := h.runPair(t,
		[]string{"--name", "alice-cli", filepath.Join(src, "x")},
		dst, nil, "y\n")
	r.requireSuccess(t)
	if !strings.Contains(r.receiverErr, "alice-cli") {
		t.Fatalf("receiver prompt missing --name override; stderr:\n%s", r.receiverErr)
	}
}

func TestReceive_InteractiveYes(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "z")
	if err := os.WriteFile(srcFile, []byte("z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.runPair(t, []string{srcFile}, dst, nil, "y\n")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "z"))
}

// --------------------------------------------------------------------
// UX flags
// --------------------------------------------------------------------

// In --quiet mode the sender prints exactly the bare code on stdout
// and nothing on stderr.
func TestUX_QuietStdoutIsBareCode(t *testing.T) {
	requireE2E(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSender(t, h.newXDG(t), "--quiet", filepath.Join(src, "x"))
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })

	// Wait for the code to appear on stdout, then kill the sender.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.TrimSpace(s.stdout.String()) != "" {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = s.cmd.Process.Signal(os.Interrupt)
	s.wait(t, 3*time.Second)

	got := strings.TrimSpace(s.stdout.String())
	if !codeRegex.MatchString(got) || got != codeRegex.FindString(got) {
		t.Fatalf("--quiet stdout: expected bare code, got %q", got)
	}
	for _, banned := range []string{"Sending ", "Waiting for"} {
		if strings.Contains(s.stderr.String(), banned) {
			t.Fatalf("--quiet stderr leaked %q:\n%s", banned, s.stderr.String())
		}
	}
}

// End-to-end --quiet: both sides should produce zero bytes on stderr
// and a byte-identical file at the destination.
func TestUX_QuietE2E(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 128*1024)

	xdg := h.newXDG(t)
	s := h.startSender(t, xdg, "--quiet", srcFile)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })

	code := s.waitForCode(t, 5*time.Second)

	rOut, rErr, exit := h.runFsendIn(t, xdg, dst, "--yes", "--quiet", code)
	senderExit := s.wait(t, 5*time.Second)

	if senderExit != 0 || exit != 0 {
		t.Fatalf("exit codes: sender=%d receiver=%d", senderExit, exit)
	}
	if s.stderr.String() != "" {
		t.Fatalf("sender stderr should be empty under --quiet:\n%s", s.stderr.String())
	}
	if rErr != "" {
		t.Fatalf("receiver stderr should be empty under --quiet:\n%s", rErr)
	}
	_ = rOut
	assertFilesEqual(t, srcFile, filepath.Join(dst, "p.bin"))
}

// --debug should be accepted (sender survives long enough to emit a
// code). We don't assert on the verbose content — just that the flag
// doesn't crash the sender pre-pair.
func TestUX_DebugFlagAccepted(t *testing.T) {
	requireE2E(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "y"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := h.startSender(t, h.newXDG(t), "--debug", filepath.Join(src, "y"))
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })

	_ = s.waitForCode(t, 5*time.Second)
	_ = s.cmd.Process.Signal(os.Interrupt)
	s.wait(t, 3*time.Second)
}

// --------------------------------------------------------------------
// Force-mode dispatch
// --------------------------------------------------------------------

func TestDispatch_ForceSend(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "x")
	if err := os.WriteFile(srcFile, []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.runPair(t, []string{"--send", srcFile}, dst, []string{"--yes"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "x"))
}

func TestDispatch_ForceReceive(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "z")
	if err := os.WriteFile(srcFile, []byte("z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.runPair(t, []string{srcFile}, dst, []string{"--receive", "--yes"}, "")
	r.requireSuccess(t)
	assertFilesEqual(t, srcFile, filepath.Join(dst, "z"))
}

// TestPassword_NoBarCollisionOnPrompt drives the receiver's full
// interactive flow when the sender used --pass and pins two related
// UX properties:
//
//  1. Prompt ordering: the "Save to ...?" confirmation comes first,
//     then the password prompt. This matches the rest of the receiver
//     UX (decide whether you want it, then go through gates).
//  2. No progress-bar collision: the bar is materialized lazily on the
//     first chunk, so mpb's stderr repaint can't overlap the password
//     input line. A regression would re-introduce a garbled line like
//     "Password required by sender:   0 % [---]   0.00 b" — what the
//     original UX bug report flagged.
func TestPassword_NoBarCollisionOnPrompt(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "secret.txt")
	if err := os.WriteFile(srcFile, []byte("classified payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Receiver stdin order matches the prompt order: "Y" for the save
	// confirmation, then the password line. Both readers use a fresh
	// bufio.Reader on os.Stdin per call, but feeding them in order
	// keeps the bytes aligned.
	r := h.runPair(t,
		[]string{"--pass=swordfish", srcFile},
		dst,
		nil, // no extra receiver flags — exercise the full interactive path
		"Y\nswordfish\n",
	)
	r.requireSuccess(t)

	saveIdx := strings.Index(r.receiverErr, "Save to")
	passwordIdx := strings.Index(r.receiverErr, "Password required by sender:")
	if saveIdx < 0 {
		t.Fatalf("save prompt not found in receiver stderr:\n%s", r.receiverErr)
	}
	if passwordIdx < 0 {
		t.Fatalf("password prompt not found in receiver stderr:\n%s", r.receiverErr)
	}
	if saveIdx >= passwordIdx {
		t.Fatalf("save prompt (idx %d) should appear before password prompt (idx %d):\n%s",
			saveIdx, passwordIdx, r.receiverErr)
	}

	// Bar collision regression: the line containing the password prompt
	// must not also contain progress-bar tokens. "% [" is the leading
	// fragment mpb emits ("  X % [######...").
	for _, line := range strings.Split(r.receiverErr, "\n") {
		if strings.Contains(line, "Password required by sender:") && strings.Contains(line, "% [") {
			t.Fatalf("progress bar rendered on password prompt line: %q", line)
		}
	}

	assertFilesEqual(t, srcFile, filepath.Join(dst, "secret.txt"))
}

// runFsendIn runs fsend in dir with args (no positional code prepended).
// Used by tests that drove the sender manually and need a vanilla
// receiver call.
func (hh *harness) runFsendIn(t *testing.T, xdgHome, dir string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := hh.fsendCmd(xdgHome, args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), exitCodeOf(t, err)
}
