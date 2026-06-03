// Package fserrors defines the user-facing error catalog for fsend.
//
// Each sentinel error here corresponds to a numbered entry in
// docs/ux/help-text.md. The User function maps any wrapped error back to a
// catalog entry (with exit code, message, and action hint) so the CLI can
// render consistent, helpful errors regardless of where in the call stack
// they originated.
//
// Design rules (see docs/ux/help-text.md):
//   - First line says what failed in user terms.
//   - Second line gives an actionable next step.
//   - Never blame the user.
//   - Exit codes are stable from v0.1.0 onward.
package fserrors

import (
	"errors"
	"fmt"
)

// Sentinel errors. Wrap these with fmt.Errorf("…: %w", Err…) so the catalog
// renderer can find them with errors.Is.
var (
	// E001
	ErrServerUnreachable = errors.New("server unreachable")
	// E002
	ErrCodeNotFound = errors.New("code not found")
	// E003
	ErrCodeAlreadyClaimed = errors.New("code already claimed")
	// E004
	ErrInvalidCodeFormat = errors.New("invalid code format")
	// E005
	ErrWrongPassword = errors.New("wrong password")
	// E006
	ErrReceiverDeclined = errors.New("receiver declined")
	// E007
	ErrPromptTimeout = errors.New("prompt timeout")
	// E008
	ErrDiskFull = errors.New("disk full")
	// E009
	ErrWriteFailed = errors.New("write failed")
	// E010
	ErrReadFailed = errors.New("read failed")
	// E011
	ErrHashMismatch = errors.New("file hash mismatch")
	// E012
	ErrPathTraversal = errors.New("path traversal rejected")
	// E013
	ErrTargetExists = errors.New("target file exists")
	// E014
	ErrConnectFailed = errors.New("could not connect to peer")
	// E015
	ErrProtocolError = errors.New("protocol error")
	// E016 — warning only, no exit
	ErrConfigCorrupted = errors.New("config corrupted")
	// E017
	ErrRateLimited = errors.New("rate limited")
	// E018
	ErrServerRetired = errors.New("server retired")
)

// Entry is one row of the user-facing error catalog.
type Entry struct {
	Code    string // e.g., "E001"
	Exit    int    // process exit code
	Message string // first line, what went wrong in user terms
	Action  string // second (and subsequent) lines, what to do next
}

// Format renders the entry as a multi-line message ready for stderr.
// Format does not add the leading "✗ "; the caller is expected to add color
// and the cross/check glyph appropriate to the output mode.
func (e Entry) Format() string {
	if e.Action == "" {
		return e.Message
	}
	return e.Message + "\n  " + e.Action
}

// catalog maps each sentinel to its presentation. Keep in sync with
// docs/ux/help-text.md.
//
// Placeholders like {addr}, {code}, {path} are filled in by the caller via
// fmt.Sprintf when known; we use plain text so unknown contexts still render.
var catalog = map[error]Entry{
	ErrServerUnreachable: {
		Code: "E001", Exit: 2,
		Message: "Could not reach rendezvous server (timeout).",
		Action: "Check your internet connection, or use a different server:\n" +
			"    fsend --connect <host:port>",
	},
	ErrCodeNotFound: {
		Code: "E002", Exit: 3,
		Message: "That code was not found.",
		Action:  "Ask the sender to re-run their command — codes expire after 60 seconds.",
	},
	ErrCodeAlreadyClaimed: {
		Code: "E003", Exit: 3,
		Message: "That code has already been claimed by another receiver.",
		Action:  "Ask the sender to generate a fresh code.",
	},
	ErrInvalidCodeFormat: {
		Code: "E004", Exit: 4,
		Message: "Invalid code format.",
		Action:  "Codes look like: abc-defgh-jkm",
	},
	ErrWrongPassword: {
		Code: "E005", Exit: 5,
		Message: "Wrong password.",
		Action:  "Double-check with the sender and run the command again.",
	},
	ErrReceiverDeclined: {
		Code: "E006", Exit: 6,
		Message: "Receiver declined the transfer.",
	},
	ErrPromptTimeout: {
		Code: "E007", Exit: 7,
		Message: "No response received within 30 seconds. Transfer aborted.",
	},
	ErrDiskFull: {
		Code: "E008", Exit: 8,
		Message: "Not enough disk space.",
		Action:  "Free up space or use --out <dir> to save somewhere else.",
	},
	ErrWriteFailed: {
		Code: "E009", Exit: 9,
		Message: "Could not write to the target file.",
		Action:  "Try --out <dir> to save somewhere writable.",
	},
	ErrReadFailed: {
		Code: "E010", Exit: 10,
		Message: "Could not read the source file.",
		Action:  "Check the file permissions and try again.",
	},
	ErrHashMismatch: {
		Code: "E011", Exit: 11,
		Message: "Transfer completed but the file did not verify correctly.",
		Action: "This usually means the sender's file changed mid-transfer, or there\n" +
			"  was data corruption. The partial file has been deleted.\n" +
			"  Ask the sender to try again.",
	},
	ErrPathTraversal: {
		Code: "E012", Exit: 12,
		Message: "Sender tried to write outside the target directory. Transfer rejected.",
		Action: "This is a security check — please report at:\n" +
			"    https://github.com/polius/fsend/issues/new?label=security",
	},
	ErrTargetExists: {
		Code: "E013", Exit: 13,
		Message: "Target file exists and --overwrite was not given.",
		Action:  "Use --overwrite to replace existing files.",
	},
	ErrConnectFailed: {
		Code: "E014", Exit: 14,
		Message: "Could not connect to the other peer, even via the relay.",
		Action: "This usually means one of:\n" +
			"    - The relay server is unreachable from your network\n" +
			"    - The other peer's connection dropped\n" +
			"    - Your firewall blocks UDP traffic\n" +
			"  Try: fsend --connect <different-server> or run with --debug for details.",
	},
	ErrProtocolError: {
		Code: "E015", Exit: 15,
		Message: "Protocol error talking to the other peer.",
		Action:  "Both sides must run a compatible fsend version.",
	},
	ErrConfigCorrupted: {
		Code: "E016", Exit: 0, // warning, not fatal
		Message: "Your config file is invalid. Falling back to defaults.",
		Action:  "To reset:  fsend --connect default",
	},
	ErrRateLimited: {
		Code: "E017", Exit: 17,
		Message: "Too many attempts from your network — rate limit hit on the server.",
		Action:  "Wait a minute and try again, or use --connect to use a different server.",
	},
	ErrServerRetired: {
		Code: "E018", Exit: 18,
		Message: "The default server (fs.alzina.dev) has been retired.",
		Action: "Switch to a different server with:\n" +
			"    fsend --connect <host:port>\n" +
			"  Or self-host: https://github.com/polius/fsend#self-hosting",
	},
}

// Lookup returns the catalog entry for the sentinel (or its wrapped chain),
// plus ok=true if a match was found. If no sentinel matches, returns the
// "catchall" entry (E099) and ok=false — callers can use the boolean to
// decide whether to include the underlying error text under DEBUG.
func Lookup(err error) (Entry, bool) {
	if err == nil {
		return Entry{}, false
	}
	for sentinel, entry := range catalog {
		if errors.Is(err, sentinel) {
			return entry, true
		}
	}
	return Entry{
		Code:    "E099",
		Exit:    99,
		Message: fmt.Sprintf("Unexpected error: %v", err),
		Action: "Please report this at:\n" +
			"    https://github.com/polius/fsend/issues\n" +
			"  Include the output of:  rerun your command with --debug",
	}, false
}

// IsWarning reports whether the error is a non-fatal warning (Exit==0).
func IsWarning(err error) bool {
	entry, ok := Lookup(err)
	return ok && entry.Exit == 0
}
