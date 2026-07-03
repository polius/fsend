package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// jsonLines parses stdout as NDJSON, failing on any non-JSON line — the
// --json contract is that stdout carries nothing else.
func jsonLines(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("non-JSON line on --json stdout: %q (%v)", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestJSON_SendReceiveHappyPath(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024)

	// --quiet on the sender: the code event must replace the bare-code
	// line, keeping stdout pure NDJSON.
	r := h.runPair(t, []string{srcFile, "--json", "--quiet"}, dst, []string{"--json", "--yes"}, "")
	if r.senderExitCode != 0 || r.receiverExitCode != 0 {
		t.Fatalf("exits: sender=%d receiver=%d\nsender stderr:\n%s\nreceiver stderr:\n%s",
			r.senderExitCode, r.receiverExitCode, r.senderErr, r.receiverErr)
	}

	sev := jsonLines(t, r.senderOut)
	if len(sev) != 2 || sev[0]["event"] != "code" || sev[1]["event"] != "done" {
		t.Fatalf("sender events = %v, want [code done]", sev)
	}
	if sev[0]["code"] != r.code {
		t.Errorf("code event carries %v, harness paired on %q", sev[0]["code"], r.code)
	}
	done := sev[1]
	if done["ok"] != true || done["role"] != "sender" || done["files_sent"] != float64(1) || done["exit"] != float64(0) {
		t.Errorf("sender done event: %v", done)
	}

	rev := jsonLines(t, r.receiverOut)
	if len(rev) != 1 || rev[0]["event"] != "done" {
		t.Fatalf("receiver events = %v, want [done]", rev)
	}
	rd := rev[0]
	if rd["ok"] != true || rd["role"] != "receiver" || rd["files_saved"] != float64(1) || rd["dir"] == nil {
		t.Errorf("receiver done event: %v", rd)
	}
	if rd["route"] == "" || rd["route"] == nil {
		t.Errorf("established transfer must report a route: %v", rd)
	}
}

// A kept differing file must surface in both done events: receiver
// ok=false error=E013 exit=13, sender ok (its exit stays 0) but files_kept=1.
func TestJSON_KeptFilesReported(t *testing.T) {
	requireE2E(t)
	src, dst := t.TempDir(), t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 64*1024)
	if err := os.WriteFile(filepath.Join(dst, "p.bin"), []byte("PREEXISTING"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := h.runPair(t, []string{srcFile, "--json"}, dst, []string{"--json", "--yes"}, "")

	rd := jsonLines(t, r.receiverOut)
	last := rd[len(rd)-1]
	if last["ok"] != false || last["error"] != "E013" || last["exit"] != float64(13) || last["files_kept"] != float64(1) {
		t.Errorf("receiver done event should report the kept file: %v", last)
	}
	if r.receiverExitCode != 13 {
		t.Errorf("receiver exit = %d, want 13", r.receiverExitCode)
	}
	sd := jsonLines(t, r.senderOut)
	sdone := sd[len(sd)-1]
	if sdone["files_kept"] != float64(1) || sdone["files_sent"] != float64(0) {
		t.Errorf("sender done event should report the kept file: %v", sdone)
	}
}

// A --text payload rides in the done event, never on raw stdout.
func TestJSON_TextPayloadInEvent(t *testing.T) {
	requireE2E(t)
	dst := t.TempDir()
	r := h.runPair(t, []string{"--text", "hello json", "--json"}, dst, []string{"--json", "--yes"}, "")
	if r.receiverExitCode != 0 {
		t.Fatalf("receiver exit = %d\nstderr:\n%s", r.receiverExitCode, r.receiverErr)
	}
	rd := jsonLines(t, r.receiverOut)
	last := rd[len(rd)-1]
	if last["text"] != "hello json" {
		t.Errorf("text payload should ride in the done event: %v", last)
	}
	if strings.Contains(r.receiverOut, "\nhello json\n") || strings.HasPrefix(r.receiverOut, "hello json") {
		t.Errorf("raw payload leaked onto --json stdout:\n%s", r.receiverOut)
	}
}

// Failures that never reach a summary still end with a done event — here a
// usage error, which exits before any pairing.
func TestJSON_UsageErrorEmitsDone(t *testing.T) {
	requireE2E(t)
	src := t.TempDir()
	srcFile := filepath.Join(src, "p.bin")
	writeRandom(t, srcFile, 1024)
	stdout, _, exit := h.runFsend(t, h.newXDG(t), srcFile, "--json", "--preview")
	if exit != 24 {
		t.Fatalf("exit = %d, want 24", exit)
	}
	ev := jsonLines(t, stdout)
	if len(ev) != 1 || ev[0]["event"] != "done" || ev[0]["ok"] != false || ev[0]["error"] != "E024" {
		t.Errorf("usage failure should emit a done event: %v", ev)
	}
}
