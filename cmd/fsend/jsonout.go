package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
)

// --json turns stdout into NDJSON: one JSON object per line, schema
// versioned by "v" and evolved additively only (documented in
// docs/usage.md). Human output stays on stderr, untouched.
//
// Two events exist:
//   - "code": the sender's pairing code, emitted as soon as it is issued
//     so a script can relay it before the transfer completes.
//   - "done": the final outcome, emitted exactly once per run — from the
//     summary path when the transfer reached one (rich: bytes/files/route),
//     or from renderError otherwise (ok/error/exit only).
//
// jsonOut is package-level state, mirroring errorRole: renderError
// runs after the transfer stack has unwound, and the once-guard is what
// lets the rich summary event win over the generic failure event.
var jsonOut struct {
	mu      sync.Mutex
	enabled bool
	done    bool
}

func jsonEnable() {
	jsonOut.mu.Lock()
	jsonOut.enabled = true
	jsonOut.mu.Unlock()
}

func jsonEnabled() bool {
	jsonOut.mu.Lock()
	defer jsonOut.mu.Unlock()
	return jsonOut.enabled
}

// jsonEvent marshals and prints one NDJSON line. Events are small structs
// of strings/ints; a marshal failure is a programming error, but stdout is
// the machine channel so it must never carry a half-written line — drop
// the event instead.
func jsonEvent(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(os.Stdout, string(b))
}

type jsonCodeEvent struct {
	V     int    `json:"v"`
	Event string `json:"event"`
	Code  string `json:"code"`
}

// jsonDoneEvent is the terminal event. Omitted fields mean "not known at
// this exit path" (a failure before the summary has no byte counts; a
// flag-parse failure has no role yet — documented in docs/usage.md).
type jsonDoneEvent struct {
	V          int    `json:"v"`
	Event      string `json:"event"`
	Ok         bool   `json:"ok"`
	Role       string `json:"role,omitempty"`
	Error      string `json:"error,omitempty"` // catalog code, e.g. "E013"
	Exit       int    `json:"exit"`
	BytesTotal *int64 `json:"bytes_total,omitempty"`
	BytesMoved *int64 `json:"bytes_moved,omitempty"`
	// Sender: files_sent / files_skipped (receiver-declined, reason
	// unknown for old peers) / files_kept. Receiver: files_saved /
	// files_up_to_date / files_kept.
	FilesSent    *int   `json:"files_sent,omitempty"`
	FilesSkipped *int   `json:"files_skipped,omitempty"`
	FilesSaved   *int   `json:"files_saved,omitempty"`
	FilesSame    *int   `json:"files_up_to_date,omitempty"`
	FilesKept    *int   `json:"files_kept,omitempty"`
	DurationMS   *int64 `json:"duration_ms,omitempty"`
	Route        string `json:"route,omitempty"`
	Dir          string `json:"dir,omitempty"`  // receiver: where files landed
	Text         string `json:"text,omitempty"` // receiver: --text payload (replaces raw stdout)
}

func jsonEmitCode(code string) {
	jsonOut.mu.Lock()
	on := jsonOut.enabled
	jsonOut.mu.Unlock()
	if on {
		jsonEvent(jsonCodeEvent{V: 1, Event: "code", Code: code})
	}
}

// jsonEmitDone emits the done event once; later calls are no-ops.
func jsonEmitDone(ev jsonDoneEvent) {
	jsonOut.mu.Lock()
	defer jsonOut.mu.Unlock()
	if !jsonOut.enabled || jsonOut.done {
		return
	}
	jsonOut.done = true
	ev.V, ev.Event = 1, "done"
	jsonEvent(ev)
}

// jsonDoneFromErr builds the generic failure event renderError emits when
// no summary ran. err == nil is a clean exit some caller narrated itself.
// role is errorRole — "" (field omitted) when the run failed before a
// send/receive path was entered, e.g. a flag-parse error.
func jsonDoneFromErr(err error, exit int, role string) jsonDoneEvent {
	ev := jsonDoneEvent{Ok: err == nil && exit == 0, Role: role, Exit: exit}
	if err != nil {
		entry, _ := fserrors.Lookup(err)
		ev.Error = entry.Code
	}
	return ev
}

// jsonRoute maps a connection path to its stable machine name; "" (omitted)
// when the path was never established.
func jsonRoute(k connpath.Kind) string {
	switch k {
	case connpath.KindLocal:
		return "local"
	case connpath.KindDirectNAT:
		return "direct"
	case connpath.KindRelay:
		return "relay"
	}
	return ""
}

func ptr64(v int64) *int64 { return &v }
func ptrInt(v int) *int    { return &v }

func msPtr(d time.Duration) *int64 { return ptr64(d.Milliseconds()) }
