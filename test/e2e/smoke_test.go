package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCLI_Help(t *testing.T) {
	requireE2E(t)
	out, _, code := h.runFsend(t, h.newXDG(t), "--help")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(strings.ToLower(out), "usage") &&
		!strings.Contains(strings.ToLower(out), "examples") {
		t.Fatalf("--help did not look like help:\n%s", out)
	}
}

func TestCLI_Version(t *testing.T) {
	requireE2E(t)
	out, _, code := h.runFsend(t, h.newXDG(t), "--version")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.HasPrefix(out, "fsend ") {
		t.Fatalf("--version output: %q", out)
	}
}

func TestCLI_HelpFlagShowsHelp(t *testing.T) {
	// Bare `fsend` with no args only prints help when stdin is a TTY;
	// when stdin is redirected (the e2e harness wires it to /dev/null
	// via exec.Cmd defaults), dispatch correctly treats that as
	// "send from stdin" and waits for a peer. We exercise the rendered
	// help surface via --help, which is independent of stdin shape.
	requireE2E(t)
	out, errOut, code := h.runFsend(t, h.newXDG(t), "--help")
	combined := strings.ToLower(out + errOut)
	if code != 0 {
		t.Fatalf("exit %d (expected 0 for --help)", code)
	}
	if !strings.Contains(combined, "usage") && !strings.Contains(combined, "examples") {
		t.Fatalf("expected help text on --help:\n%s\n%s", out, errOut)
	}
}

func TestCLI_InvalidCodeExits4(t *testing.T) {
	requireE2E(t)
	_, _, code := h.runFsend(t, h.newXDG(t), "--receive", "nope")
	if code != 4 {
		t.Fatalf("expected exit 4 (E004), got %d", code)
	}
}

func TestCLI_UnknownFlagFails(t *testing.T) {
	requireE2E(t)
	_, _, code := h.runFsend(t, h.newXDG(t), "--nosuchflag")
	if code == 0 {
		t.Fatal("expected non-zero exit on unknown flag")
	}
}

// --------------------------------------------------------------------
// fsend server (subcommand)
// --------------------------------------------------------------------

func TestServer_Help(t *testing.T) {
	requireE2E(t)
	out, err := exec.Command(h.fsendBin, "server", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("server --help: %v\n%s", err, out)
	}
	lower := strings.ToLower(string(out))
	if !strings.Contains(lower, "usage") && !strings.Contains(lower, "configuration") {
		t.Fatalf("server --help did not look like help:\n%s", out)
	}
}

func TestServer_HealthCheck(t *testing.T) {
	requireE2E(t)
	cmd := exec.Command(h.fsendBin, "server", "--health-check")
	cmd.Env = append(os.Environ(), "FSEND_PAIRING_ADDR=:"+strconv.Itoa(h.httpPort))
	if err := cmd.Run(); err != nil {
		t.Fatalf("--health-check exit: %v", err)
	}
}

// TestServer_PasswordGate spins up a second server process protected
// with FSEND_SERVER_PASSWORD and confirms:
//   - a client with no/wrong password gets exit 28 (E028) on Create
//   - a client that has set the matching password via `fsend --connect`
//     can pair and transfer end-to-end
//
// Uses the same binary the rest of the suite built; the second server
// runs on a fresh free port so it doesn't collide with h.
func TestServer_PasswordGate(t *testing.T) {
	requireE2E(t)

	httpPort, err := freePort()
	if err != nil {
		t.Fatalf("pick http port: %v", err)
	}
	udpPort, err := freePort()
	if err != nil {
		t.Fatalf("pick udp port: %v", err)
	}

	cmd := exec.Command(h.fsendBin, "server")
	cmd.Env = append(os.Environ(),
		"FSEND_PAIRING_ADDR=:"+strconv.Itoa(httpPort),
		"FSEND_RELAY_ADDR=:"+strconv.Itoa(udpPort),
		"FSEND_LOG_LEVEL=warn",
		"FSEND_SERVER_PASSWORD=swordfish",
		"FSEND_PAIRING_MAX_NEW_SESSIONS_PER_IP_PER_MIN=10000",
		"FSEND_PAIRING_MAX_SESSIONS_PER_IP=1000",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start guarded server: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if err := waitTCP("127.0.0.1:"+strconv.Itoa(httpPort), 5*time.Second); err != nil {
		t.Fatalf("guarded server not ready: %v", err)
	}
	addr := "127.0.0.1:" + strconv.Itoa(httpPort)

	// Send forced to the server-only path so the LAN race can't sidestep
	// the password check. --quiet keeps the sender's stderr clean so the
	// code-detection regex is the only thing the test reads.
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}

	// --- Step 1: receiver configured with no password → E028 ---
	xdgNoPw := h.newXDG(t)
	if _, _, code := h.runFsend(t, xdgNoPw, "--connect", addr); code != 0 {
		t.Fatalf("--connect (no pw) exit %d", code)
	}
	_, _, code := h.runFsend(t, xdgNoPw, "--receive", "abc-defg-jkm")
	if code != 28 {
		t.Errorf("receiver without password: exit %d, want 28 (E028)", code)
	}

	// --- Step 2: with the matching password the full pair succeeds ---
	xdg := h.newXDG(t)
	if _, _, code := h.runFsend(t, xdg, "--connect", addr+",swordfish"); code != 0 {
		t.Fatalf("--connect with pw exit %d", code)
	}

	s := h.startSender(t, xdg, "--mode", "direct", filepath.Join(src, "x"))
	t.Cleanup(func() { _ = s.cmd.Process.Kill() })
	codeStr := s.waitForCode(t, 5*time.Second)

	dst := t.TempDir()
	recvCmd := h.fsendCmd(xdg, "--yes", codeStr)
	recvCmd.Dir = dst
	if out, err := recvCmd.CombinedOutput(); err != nil {
		t.Fatalf("receiver: %v\n%s", err, out)
	}
	if got, _ := os.ReadFile(filepath.Join(dst, "x")); string(got) != "payload" {
		t.Errorf("payload mismatch: %q", got)
	}
	if exit := s.wait(t, 5*time.Second); exit != 0 {
		t.Errorf("sender exit %d", exit)
	}
}

// --------------------------------------------------------------------
// --connect (config persistence)
// --------------------------------------------------------------------

func TestConnect_SetsPersistedServer(t *testing.T) {
	requireE2E(t)
	xdg := h.newXDG(t)

	_, _, code := h.runFsend(t, xdg, "--connect", "relay.example.com:443")
	if code != 0 {
		t.Fatalf("--connect set: exit %d", code)
	}
	got, err := os.ReadFile(filepath.Join(xdg, "fsend", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(got), "relay.example.com:443") {
		t.Fatalf("expected server persisted in config:\n%s", got)
	}
}

func TestConnect_DefaultClearsServer(t *testing.T) {
	requireE2E(t)
	xdg := h.newXDG(t)

	_, _, code := h.runFsend(t, xdg, "--connect", "relay.example.com:443")
	if code != 0 {
		t.Fatalf("set: exit %d", code)
	}
	_, _, code = h.runFsend(t, xdg, "--connect", "default")
	if code != 0 {
		t.Fatalf("default: exit %d", code)
	}
	got, err := os.ReadFile(filepath.Join(xdg, "fsend", "config.json"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(got), "relay.example.com:443") {
		t.Fatalf("server should have been cleared:\n%s", got)
	}
}
