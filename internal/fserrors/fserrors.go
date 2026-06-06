// Package fserrors defines the user-facing error catalog for fsend.
//
// The User function maps any wrapped error back to a catalog entry
// (with exit code, message, and action hint) so the CLI can render
// consistent, helpful errors regardless of where in the call stack they
// originated.
//
// Design rules:
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
	// E006
	ErrReceiverDeclined = errors.New("receiver declined")
	// E007 — the rendezvous server reaped the sender's session before a
	// receiver paired (server-side UnpairedTTL hit). Distinct from E002
	// "code not found" because the failure is on the sender's side; the
	// receiver had nothing to look up to begin with.
	ErrSessionExpired = errors.New("session expired")
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
	// E019 — sender's prefix does not match receiver's partial. Almost always
	// means the source changed between attempts.
	ErrPartialMismatch = errors.New("partial does not match source")
	// E020 — transfer was interrupted but is recoverable; surfaced when
	// retries are exhausted.
	ErrTransientFailure = errors.New("transient transfer failure")
	// E021 — receiver did not match the sender's --pass challenge.
	ErrWrongPassword = errors.New("wrong password")
	// E022 — peers did not agree on the short code (or someone in the
	// middle tried to MITM): SPAKE2-derived key + TLS exporter mismatch.
	ErrPeerAuthFailed = errors.New("peer authentication failed")
	// E023 — relay's per-session byte cap was hit and the allocation
	// was torn down. Tells the user *why* the transfer stopped, instead
	// of "connection interrupted, retrying" forever.
	ErrRelayCapHit = errors.New("relay byte cap reached")
	// E024 — any CLI usage error (bad flag, bad arg shape, conflicting
	// modes). Routine; never asks the user to file a bug.
	ErrUsage = errors.New("usage error")
	// E025 — the local source path the user tried to send doesn't exist.
	// Distinct from a read failure on an existing file (E010).
	ErrSourceNotFound = errors.New("source not found")
	// E026 — the user cancelled (Ctrl-C / SIGTERM). Exit 130 is the
	// shell convention for "terminated by SIGINT".
	ErrUserCancelled = errors.New("cancelled by user")
	// E027 — could not open the local-network listener (typically the
	// deterministic per-code UDP port is already bound, or mDNS init
	// failed). Distinct from E014 ("could not reach the other peer")
	// because the failure is on this side, before any peer is involved.
	ErrLANListenerFailed = errors.New("local network listener failed")
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

// Render is the user-facing form: "[Exxx] <message>\n  <action>".
// The catalog code is included as a stable identifier users can quote in
// bug reports or scripts (exit codes are unique per entry, but a textual
// tag is easier to refer to in chat).
func (e Entry) Render() string {
	if e.Code == "" {
		return e.Format()
	}
	if e.Action == "" {
		return "[" + e.Code + "] " + e.Message
	}
	return "[" + e.Code + "] " + e.Message + "\n  " + e.Action
}

// catalog maps each sentinel to its presentation.
//
// Exit codes are kept in lockstep with the Exxx number (E001 → exit 1,
// E024 → exit 24, etc.) so scripts can match on either. The only
// exceptions are E016 (a non-fatal warning, exit 0) and E026 (Ctrl-C,
// exit 130 by shell convention). Codes are stable from v1.0 onward.
//
// Placeholders like {addr}, {code}, {path} are filled in by the caller via
// fmt.Sprintf when known; we use plain text so unknown contexts still render.
var catalog = map[error]Entry{
	ErrServerUnreachable: {
		Code: "E001", Exit: 1,
		Message: "Could not reach the server (timeout).",
		Action: "Check your internet connection, or use a different server:\n" +
			"    fsend --connect <host:port>",
	},
	ErrCodeNotFound: {
		Code: "E002", Exit: 2,
		Message: "That code was not found.",
		Action:  "Double-check the code with the sender and make sure their fsend is still running.",
	},
	ErrCodeAlreadyClaimed: {
		Code: "E003", Exit: 3,
		Message: "That code has already been claimed by another receiver.",
		Action:  "Ask the sender to generate a fresh code.",
	},
	ErrInvalidCodeFormat: {
		Code: "E004", Exit: 4,
		Message: "Invalid code format.",
		Action:  "Codes look like: abc-defg-jkm",
	},
	ErrReceiverDeclined: {
		Code: "E006", Exit: 6,
		Message: "Receiver declined the transfer.",
	},
	ErrSessionExpired: {
		Code: "E007", Exit: 7,
		Message: "The session expired on the server before a receiver paired.",
		Action:  "Re-run your command to publish a fresh code.",
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
	ErrPartialMismatch: {
		Code: "E019", Exit: 19,
		Message: "The source file changed since the last attempt — stale partial discarded.",
		Action:  "Run the same command again to fetch a fresh copy from scratch.",
	},
	ErrTransientFailure: {
		Code: "E020", Exit: 20,
		Message: "Transfer was interrupted and retries did not recover.",
		Action: "Check your network connection and try again — fsend will resume\n" +
			"  from where it left off.",
	},
	ErrWrongPassword: {
		Code: "E021", Exit: 21,
		Message: "Wrong password. Transfer aborted.",
		Action: "Ask the sender for the correct password and run again. Codes are\n" +
			"  one-shot — the sender may need to restart to issue a fresh code.",
	},
	ErrPeerAuthFailed: {
		Code: "E022", Exit: 22,
		Message: "Could not authenticate the other peer.",
		Action: "Either the codes don't match or something in the network path\n" +
			"  tampered with the connection. Re-share the code and try again.",
	},
	ErrRelayCapHit: {
		Code: "E023", Exit: 23,
		Message: "The relay server's per-session byte cap was reached. Transfer aborted.",
		Action: "Same-LAN and NAT-hole-punched transfers are uncapped; only the relay\n" +
			"  fallback is metered. Workarounds:\n" +
			"    - Send from a different network so the peers can hole-punch directly.\n" +
			"    - Self-host your own server (`fsend server`) and raise FSEND_MAX_RELAY_BYTES_PER_SESSION.",
	},
	ErrUsage: {
		Code: "E024", Exit: 24,
		Message: "Invalid usage.",
		Action:  "Run `fsend --help` for the full command surface.",
	},
	ErrSourceNotFound: {
		Code: "E025", Exit: 25,
		Message: "Source not found.",
		Action:  "Check the path and try again.",
	},
	ErrUserCancelled: {
		Code: "E026", Exit: 130,
		Message: "Cancelled.",
	},
	ErrLANListenerFailed: {
		Code: "E027", Exit: 27,
		Message: "Could not open the local-network listener.",
		Action: "Another fsend (or another program) may already be using the port.\n" +
			"  Try again — most codes use a different port.",
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
		Action: "Re-run with --debug and include the output when filing the issue:\n" +
			"    https://github.com/polius/fsend/issues",
	}, false
}

// IsWarning reports whether the error is a non-fatal warning (Exit==0).
func IsWarning(err error) bool {
	entry, ok := Lookup(err)
	return ok && entry.Exit == 0
}

// Chain returns the full wrap chain of err as a slice of error strings,
// outermost first. Used by --debug rendering to expose the underlying
// technical details after the friendly catalog message.
func Chain(err error) []string {
	var out []string
	for e := err; e != nil; e = errors.Unwrap(e) {
		out = append(out, e.Error())
	}
	return out
}
