package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
	cmd.Env = append(os.Environ(), "FSEND_HTTP_ADDR=:"+strconv.Itoa(h.httpPort))
	if err := cmd.Run(); err != nil {
		t.Fatalf("--health-check exit: %v", err)
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
