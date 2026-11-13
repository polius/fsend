package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/fserrors"
)

// A failure before any transfer path is entered (flag-parse error) has no
// role; the done event must omit the field rather than fabricate "receiver".
func TestJSONDoneFromErr_RoleOmittedWhenUnknown(t *testing.T) {
	b, err := json.Marshal(jsonDoneFromErr(fserrors.ErrUsage, 24, ""))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"role"`) {
		t.Errorf("undetermined role must be omitted, got %s", b)
	}
	if !strings.Contains(string(b), `"error":"E024"`) {
		t.Errorf("catalog code missing: %s", b)
	}

	b, err = json.Marshal(jsonDoneFromErr(fserrors.ErrCodeNotFound, 2, "receiver"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"role":"receiver"`) {
		t.Errorf("determined role must be present, got %s", b)
	}
}

// The events between code and done: shapes are additive v1, and progress
// is throttled to one a second.
func TestJSONTransferEvents(t *testing.T) {
	jsonOut.mu.Lock()
	jsonOut.enabled, jsonOut.done = true, false
	jsonOut.mu.Unlock()
	t.Cleanup(func() {
		jsonOut.mu.Lock()
		jsonOut.enabled, jsonOut.done = false, false
		jsonOut.mu.Unlock()
	})
	out := captureStdout(t, func() {
		jsonEmitPeer("mbp", "local")
		jsonEmitAccepted()
		jsonEmitRetry(2, 3, 1500*time.Millisecond)
		var p jsonProgress
		p.emit(1, 100, "a.iso")
		p.emit(2, 100, "a.iso") // inside the interval: dropped
		p.emit(3, 0, "")        // unknown total: omitted
	})
	want := `{"v":1,"event":"peer","name":"mbp","route":"local"}
{"v":1,"event":"accepted"}
{"v":1,"event":"retry","attempt":2,"of":3,"wait_ms":1500}
{"v":1,"event":"progress","bytes":1,"total":100,"file":"a.iso"}
`
	if out != want {
		t.Errorf("events =\n%s\nwant\n%s", out, want)
	}
}
