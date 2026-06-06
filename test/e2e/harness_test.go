// Package e2e exercises the built fsend and fsend-server binaries
// end-to-end.
//
// TestMain compiles both binaries from the current working tree into a
// temp dir, starts an isolated fsend-server on loopback, and exposes a
// shared harness via the package-level h. The suite skips under
// `go test -short`.
package e2e

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// codeRegex matches the canonical 3-4-3 fsend pairing code (Crockford
// alphabet minus i/l/o).
var codeRegex = regexp.MustCompile(`[a-hjkmnp-z]{3}-[a-hjkmnp-z]{4}-[a-hjkmnp-z]{3}`)

// h is the singleton harness shared by every test. nil while running
// under -short.
var h *harness

type harness struct {
	repoDir        string
	fsendBin       string
	fsendServerBin string
	httpPort       int
	udpPort        int

	serverCmd    *exec.Cmd
	serverOutput bytes.Buffer
}

func TestMain(m *testing.M) {
	flag.Parse()
	if testing.Short() {
		os.Exit(m.Run())
	}

	var err error
	h, err = startHarness()
	if err != nil {
		fmt.Fprintln(os.Stderr, "e2e: harness setup failed:", err)
		os.Exit(1)
	}
	code := m.Run()
	h.shutdown()
	os.Exit(code)
}

func startHarness() (*harness, error) {
	repoDir, err := repoRoot()
	if err != nil {
		return nil, err
	}

	binDir, err := os.MkdirTemp("", "fsend-e2e-bin-")
	if err != nil {
		return nil, err
	}

	hh := &harness{
		repoDir:        repoDir,
		fsendBin:       filepath.Join(binDir, "fsend"),
		fsendServerBin: filepath.Join(binDir, "fsend-server"),
	}

	if err := goBuild(repoDir, hh.fsendBin, "./cmd/fsend"); err != nil {
		return nil, fmt.Errorf("build fsend: %w", err)
	}
	if err := goBuild(repoDir, hh.fsendServerBin, "./cmd/fsend-server"); err != nil {
		return nil, fmt.Errorf("build fsend-server: %w", err)
	}

	hh.httpPort, err = freePort()
	if err != nil {
		return nil, fmt.Errorf("pick http port: %w", err)
	}
	hh.udpPort, err = freePort()
	if err != nil {
		return nil, fmt.Errorf("pick udp port: %w", err)
	}

	hh.serverCmd = exec.Command(hh.fsendServerBin)
	hh.serverCmd.Env = append(os.Environ(),
		"FSEND_HTTP_ADDR=:"+strconv.Itoa(hh.httpPort),
		"FSEND_UDP_ADDR=:"+strconv.Itoa(hh.udpPort),
		"FSEND_PUBLIC_ADDR=127.0.0.1:"+strconv.Itoa(hh.udpPort),
		"FSEND_LOG_LEVEL=warn",
		// Both peers in this harness come from 127.0.0.1, so a single
		// pair burns two slots against one bucket. The suite runs
		// dozens of pairs back-to-back, especially under -count=N;
		// the production default of 30/min would throttle the harness
		// itself. Production sender/receiver come from distinct IPs
		// and each get their own bucket.
		"FSEND_MAX_NEW_SESSIONS_PER_IP_PER_MIN=10000",
		"FSEND_MAX_SESSIONS_PER_IP=1000",
	)
	hh.serverCmd.Stdout = &hh.serverOutput
	hh.serverCmd.Stderr = &hh.serverOutput

	if err := hh.serverCmd.Start(); err != nil {
		return nil, fmt.Errorf("start fsend-server: %w", err)
	}
	if err := waitTCP("127.0.0.1:"+strconv.Itoa(hh.httpPort), 5*time.Second); err != nil {
		hh.shutdown()
		return nil, fmt.Errorf("fsend-server not ready: %w\n--- server output ---\n%s",
			err, hh.serverOutput.String())
	}
	return hh, nil
}

func (hh *harness) shutdown() {
	if hh == nil || hh.serverCmd == nil || hh.serverCmd.Process == nil {
		return
	}
	_ = hh.serverCmd.Process.Kill()
	_, _ = hh.serverCmd.Process.Wait()
	_ = os.RemoveAll(filepath.Dir(hh.fsendBin))
}

// repoRoot returns the directory containing go.mod for the current module.
func repoRoot() (string, error) {
	out, err := exec.Command("go", "env", "GOMOD").Output()
	if err != nil {
		return "", fmt.Errorf("go env GOMOD: %w", err)
	}
	gomod := strings.TrimSpace(string(out))
	if gomod == "" || gomod == os.DevNull {
		return "", errors.New("not inside a Go module")
	}
	return filepath.Dir(gomod), nil
}

func goBuild(repoDir, outPath, pkg string) error {
	cmd := exec.Command("go", "build", "-o", outPath, pkg)
	cmd.Dir = repoDir
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

func waitTCP(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("no listener on %s after %s", addr, timeout)
}

// ----------------------------------------------------------------------
// Per-test helpers
// ----------------------------------------------------------------------

// requireE2E skips the calling test under `go test -short`.
func requireE2E(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("e2e: skipped in -short mode")
	}
	if h == nil {
		t.Skip("e2e: harness not initialized")
	}
}

// newXDG returns a fresh XDG_CONFIG_HOME pre-seeded with a config that
// points fsend at our local fsend-server. Use this for any fsend
// invocation so the user's real config is untouched and tests don't
// reach out to the public rendezvous.
func (hh *harness) newXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfgDir := filepath.Join(dir, "fsend")
	if err := os.MkdirAll(cfgDir, 0o700); err != nil {
		t.Fatalf("mkdir xdg: %v", err)
	}
	cfg := fmt.Sprintf(`{"schema_version":1,"server":"127.0.0.1:%d"}`, hh.httpPort)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return dir
}

// fsendCmd builds an *exec.Cmd for fsend with the given XDG_CONFIG_HOME
// and args. The caller owns stdin/stdout/stderr.
func (hh *harness) fsendCmd(xdgHome string, args ...string) *exec.Cmd {
	cmd := exec.Command(hh.fsendBin, args...)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+xdgHome)
	return cmd
}

// runFsend runs fsend to completion and returns stdout, stderr, exit code.
// Use this for non-paired invocations (smoke, --connect, --help, etc.).
func (hh *harness) runFsend(t *testing.T, xdgHome string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := hh.fsendCmd(xdgHome, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), exitCodeOf(t, err)
}

func exitCodeOf(t *testing.T, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("unexpected exec error: %v", err)
	return -1
}

// ----------------------------------------------------------------------
// Sender process: long-lived, code emitted asynchronously
// ----------------------------------------------------------------------

type senderProc struct {
	cmd    *exec.Cmd
	stdout *safeBuffer
	stderr *safeBuffer

	// waitOnce guards launching the single cmd.Wait goroutine.
	// waitDone is closed once that goroutine returns, after which waitErr
	// and cmd.ProcessState are safe to read. Concurrent calls to wait
	// observe the same channel and never race on ProcessState directly.
	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

// startSender launches fsend in the background and returns immediately.
// The caller must call wait or kill to release resources.
func (hh *harness) startSender(t *testing.T, xdgHome string, args ...string) *senderProc {
	return hh.startSenderStdin(t, xdgHome, "", args...)
}

// startSenderStdin is like startSender but pipes stdin into the sender.
func (hh *harness) startSenderStdin(t *testing.T, xdgHome, stdin string, args ...string) *senderProc {
	t.Helper()
	var r io.Reader
	if stdin != "" {
		r = strings.NewReader(stdin)
	}
	return hh.startSenderStdinReader(t, xdgHome, r, args...)
}

// startSenderStdinReader is like startSenderStdin but accepts an
// arbitrary io.Reader for stdin. Used by tests that want to drive a
// producer (e.g. a goroutine that writes in chunks with delays) into
// the sender to exercise the streaming path.
func (hh *harness) startSenderStdinReader(t *testing.T, xdgHome string, stdin io.Reader, args ...string) *senderProc {
	t.Helper()
	s := &senderProc{
		cmd:    hh.fsendCmd(xdgHome, args...),
		stdout: &safeBuffer{},
		stderr: &safeBuffer{},
	}
	if stdin != nil {
		s.cmd.Stdin = stdin
	}
	s.cmd.Stdout = s.stdout
	s.cmd.Stderr = s.stderr
	if err := s.cmd.Start(); err != nil {
		t.Fatalf("start sender: %v", err)
	}
	return s
}

// waitForCode polls the sender's stdout+stderr until a pairing code
// appears or the timeout elapses. Fails the test on timeout.
func (s *senderProc) waitForCode(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if m := codeRegex.FindString(s.stderr.String()); m != "" {
			return m
		}
		if m := codeRegex.FindString(s.stdout.String()); m != "" {
			return m
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("sender did not emit a pairing code within %s\n--- stdout ---\n%s\n--- stderr ---\n%s",
		timeout, s.stdout.String(), s.stderr.String())
	return ""
}

// signal forwards a signal to the running sender.
func (s *senderProc) signal(t *testing.T, sig os.Signal) {
	t.Helper()
	if err := s.cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal sender: %v", err)
	}
}

// wait blocks until the sender exits or timeout elapses. On timeout the
// process is killed and -1 is returned. Safe to call multiple times —
// subsequent calls observe the same waitDone channel and never touch
// cmd.ProcessState concurrently with the Wait goroutine that owns it.
func (s *senderProc) wait(t *testing.T, timeout time.Duration) int {
	t.Helper()
	s.waitOnce.Do(func() {
		s.waitDone = make(chan struct{})
		go func() {
			s.waitErr = s.cmd.Wait()
			close(s.waitDone)
		}()
	})
	select {
	case <-s.waitDone:
		return exitCodeOrZero(s.waitErr)
	case <-time.After(timeout):
		_ = s.cmd.Process.Kill()
		<-s.waitDone
		return -1
	}
}

func exitCodeOrZero(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// ----------------------------------------------------------------------
// runPair: the dominant happy-path pattern
// ----------------------------------------------------------------------

type pairResult struct {
	code             string
	senderOut        string
	senderErr        string
	receiverOut      string
	receiverErr      string
	receiverExitCode int
	senderExitCode   int
}

// runPair starts a sender with senderArgs, waits for its code, then
// runs `fsend recvArgs... <code>` in recvDir. Waits for both to exit
// and returns captured streams.
//
// Both sides use the same isolated XDG so they hit the same local
// fsend-server.
//
// recvStdin, if non-empty, is piped into the receiver — used for the
// interactive prompt cases.
func (hh *harness) runPair(t *testing.T, senderArgs []string, recvDir string, recvArgs []string, recvStdin string) pairResult {
	t.Helper()
	xdg := hh.newXDG(t)
	s := hh.startSender(t, xdg, senderArgs...)
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })

	code := s.waitForCode(t, 5*time.Second)

	recvCmd := hh.fsendCmd(xdg, append(append([]string{}, recvArgs...), code)...)
	recvCmd.Dir = recvDir
	if recvStdin != "" {
		recvCmd.Stdin = strings.NewReader(recvStdin)
	}
	var rOut, rErr bytes.Buffer
	recvCmd.Stdout = &rOut
	recvCmd.Stderr = &rErr
	recvErr := recvCmd.Run()

	senderExit := s.wait(t, 5*time.Second)

	return pairResult{
		code:             code,
		senderOut:        s.stdout.String(),
		senderErr:        s.stderr.String(),
		receiverOut:      rOut.String(),
		receiverErr:      rErr.String(),
		receiverExitCode: exitCodeOf(t, recvErr),
		senderExitCode:   senderExit,
	}
}

// requirePairSuccess fails the test unless both sides exited 0.
func (r pairResult) requireSuccess(t *testing.T) {
	t.Helper()
	if r.senderExitCode != 0 || r.receiverExitCode != 0 {
		t.Fatalf("pair failed: sender=%d receiver=%d\n--- sender stderr ---\n%s\n--- receiver stderr ---\n%s",
			r.senderExitCode, r.receiverExitCode, r.senderErr, r.receiverErr)
	}
}

// ----------------------------------------------------------------------
// File assertions
// ----------------------------------------------------------------------

// writeRandom creates path with sizeBytes of random data.
func writeRandom(t *testing.T, path string, sizeBytes int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer f.Close()
	if sizeBytes == 0 {
		return
	}
	if _, err := io.CopyN(f, rand.Reader, int64(sizeBytes)); err != nil {
		t.Fatalf("write random: %v", err)
	}
}

// assertFilesEqual fails unless a and b have identical bytes.
func assertFilesEqual(t *testing.T, a, b string) {
	t.Helper()
	ah, err := fileSum(a)
	if err != nil {
		t.Fatalf("hash %s: %v", a, err)
	}
	bh, err := fileSum(b)
	if err != nil {
		t.Fatalf("hash %s: %v", b, err)
	}
	if ah != bh {
		t.Fatalf("files differ: %s vs %s", a, b)
	}
}

func fileSum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ----------------------------------------------------------------------
// safeBuffer: concurrent-safe io.Writer used for live stderr/stdout
// capture during code polling.
// ----------------------------------------------------------------------

type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
