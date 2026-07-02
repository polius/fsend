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
	// E007 — the pairing server reaped the sender's session before a
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
	// E037 — the relay's server-wide daily byte budget is spent, so it
	// stopped forwarding until the next UTC day. Distinct from E023: the
	// limit is global, not per-session, and time-based rather than size.
	ErrRelayBudgetExhausted = errors.New("relay daily budget exhausted")
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
	// E028 — the pairing server returned 401: it's gated behind a
	// shared password and the client either didn't send one or sent the
	// wrong one.
	ErrServerAuthRequired = errors.New("server password required")
	// E029 — relay reaped the allocation because no datagrams flowed
	// for ~60s. Distinct from E020 so the user knows the server tore
	// the slot down (peer went away), not their own network.
	ErrRelayIdleTimeout = errors.New("relay session idle-timed out")
	// E030 — `fsend server` failed to start: a listener couldn't bind
	// (port already in use, or :443 without privileges) or the env-var
	// config was invalid. An operator error, not an fsend bug, so it does
	// not route through the E099 "file an issue" catchall.
	ErrServerStartup = errors.New("server failed to start")
	// E031 — the sender requires a password and the receiver had none to
	// offer (--quiet with no --pass / FSEND_PASS), so the challenge was
	// never answered. Distinct from E021: no password was entered at all.
	ErrPasswordRequired = errors.New("password required")
	// E032 — the peer deliberately cancelled (Ctrl-C) mid-transfer.
	// Distinct from E020 so the survivor knows the teardown wasn't a
	// network drop.
	ErrPeerCancelled = errors.New("peer cancelled the transfer")
	// E033 — `fsend --update` could not complete: the release lookup
	// failed or the installer exited nonzero. An environment problem,
	// not a bug, so it skips the E099 "file an issue" catchall.
	ErrUpdateFailed = errors.New("update failed")
	// E034 — the two peers speak incompatible wire-protocol versions (a
	// breaking fsend release changed the format). Resolved only by updating
	// the older side; distinct from E015 so the message can say so.
	ErrIncompatibleVersion = errors.New("incompatible fsend version")
	// E035 — `fsend --uninstall` could not remove the binary (typically a
	// privileged install dir needing sudo). A non-zero exit so scripts can
	// tell a failed uninstall from a clean one.
	ErrUninstallFailed = errors.New("uninstall failed")
	// E036 — a symlink in the send set can't be followed to real content:
	// its target is missing, unreadable, or the link cycles. fsend sends the
	// pointed-to content, so an unresolvable link is a hard stop (with the
	// path, so the user can fix or --exclude it).
	ErrUnsendableSymlink = errors.New("unsendable symlink")
)

// Entry is one row of the user-facing error catalog.
type Entry struct {
	Code    string // e.g., "E001"
	Exit    int    // process exit code
	Message string // first line, what went wrong in user terms
	Action  string // second (and subsequent) lines, what to do next

	// Sender-side wording for errors that originate on the receiver and
	// are mirrored across the wire (wrong password, overwrite refused).
	// Empty means Message/Action read correctly on both sides.
	SenderMessage string
	SenderAction  string
}

// ForSender returns the entry with sender-side wording swapped in.
func (e Entry) ForSender() Entry {
	if e.SenderMessage != "" {
		e.Message = e.SenderMessage
	}
	if e.SenderAction != "" {
		e.Action = e.SenderAction
	}
	return e
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
// exit 130 by shell convention). Codes are stable from v0.1.0 onward.
//
// Placeholders like {addr}, {code}, {path} are filled in by the caller via
// fmt.Sprintf when known; we use plain text so unknown contexts still render.

// selfHostHint closes every server-limit error: the limits are the operator's
// choice, so the surest fix is running your own server. Kept identical across
// the rate-limit entries so the guidance never drifts.
const selfHostHint = "\n  Or run your own fsend server, where you set the limits:\n" +
	"    https://github.com/polius/fsend/blob/main/docs/self-hosting.md"

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
		// Receiver-side wording: this is the decliner's own deliberate
		// choice, so don't narrate it back in the third person.
		Message:       "Declined.",
		SenderMessage: "Receiver declined the transfer.",
	},
	ErrSessionExpired: {
		Code: "E007", Exit: 7,
		Message: "Your code timed out before anyone received it.",
		Action:  "Run the command again to get a fresh code.",
	},
	ErrDiskFull: {
		Code: "E008", Exit: 8,
		Message: "Not enough disk space.",
		Action:  "Free up space or use --out <dir> to save somewhere else.",
	},
	ErrWriteFailed: {
		Code: "E009", Exit: 9,
		Message:       "Could not write to the target file.",
		Action:        "Try --out <dir> to save somewhere writable.",
		SenderMessage: "The receiver could not write the file to disk.",
		SenderAction:  "They can retry with --out <dir> to save somewhere writable.",
	},
	ErrReadFailed: {
		Code: "E010", Exit: 10,
		Message: "Could not read the source file.",
		Action:  "Check the file permissions and try again.",
	},
	ErrHashMismatch: {
		Code: "E011", Exit: 11,
		Message: "The file arrived corrupted — it doesn't match the sender's original.",
		Action: "Usually this means the source changed during the transfer, or data\n" +
			"  was damaged in transit. The partial file has been deleted.\n" +
			"  Ask the sender to try again.",
	},
	ErrPathTraversal: {
		Code: "E012", Exit: 12,
		Message: "Sender tried to write outside the target directory. Transfer rejected.",
		Action: "This is a security check — please report at:\n" +
			"    https://github.com/polius/fsend/issues/new?labels=security",
	},
	ErrTargetExists: {
		Code: "E013", Exit: 13,
		Message:       "Target file exists and --overwrite was not given.",
		Action:        "Use --overwrite to replace existing files.",
		SenderMessage: "The receiver already has a file with this name.",
		SenderAction:  "They can rerun with --overwrite to replace it.",
	},
	ErrConnectFailed: {
		Code: "E014", Exit: 14,
		Message: "Could not reach the other side.",
		Action: "fsend tried a direct connection and the server-relayed fallback;\n" +
			"  neither worked. Common causes:\n" +
			"    - The other side's fsend stopped or lost network.\n" +
			"    - A firewall on either end blocks outbound traffic.\n" +
			"    - The server is unreachable from your network.\n" +
			"  Try a different server (`fsend --connect <host:port>`)\n" +
			"  or re-run with --debug for details.",
	},
	ErrProtocolError: {
		Code: "E015", Exit: 15,
		Message: "The two sides could not agree on how to transfer.",
		Action:  "Make sure sender and receiver are on compatible fsend versions.",
	},
	ErrConfigCorrupted: {
		Code: "E016", Exit: 0, // warning, not fatal
		Message: "Your config file is invalid. Falling back to defaults.",
		Action:  "To reset:  fsend --connect default",
	},
	ErrRateLimited: {
		Code: "E017", Exit: 17,
		Message: "Too many attempts from your network — rate limit hit on the server.",
		Action: "Wait a minute and try again, or switch servers:\n" +
			"    fsend --connect <host:port>" +
			selfHostHint,
	},
	ErrServerRetired: {
		Code: "E018", Exit: 18,
		// No hardcoded host: any server (including self-hosted ones) can
		// return 410. renderError appends "Server: <addr>" as the detail.
		Message: "This server has been retired.",
		Action: "Switch to a different server with:\n" +
			"    fsend --connect <host:port>\n" +
			"  Or self-host: https://github.com/polius/fsend/blob/main/docs/self-hosting.md",
	},
	ErrPartialMismatch: {
		Code: "E019", Exit: 19,
		Message: "The source file changed since the last attempt — the incomplete download was discarded.",
		Action:  "Run the same command again to start fresh.",
	},
	ErrTransientFailure: {
		Code: "E020", Exit: 20,
		Message: "Transfer was interrupted and retries did not recover.",
		// Receiver wording: share codes are one-shot, so rerunning the
		// same `fsend <code>` yields E002 — resume needs a fresh code
		// from the sender.
		Action: "Ask the sender to run fsend again, then use the new code — the\n" +
			"  transfer will resume in this same directory.",
		SenderAction: "Check your network connection and try again — fsend will resume\n" +
			"  from where it left off.",
	},
	ErrWrongPassword: {
		Code: "E021", Exit: 21,
		Message: "Wrong password. Transfer aborted.",
		Action: "Ask the sender for the correct password and run again. Codes are\n" +
			"  one-shot — the sender may need to restart to issue a fresh code.",
		SenderMessage: "The receiver entered the wrong password. Transfer aborted.",
		SenderAction: "Run fsend again to issue a fresh code, and re-share the\n" +
			"  password with the receiver.",
	},
	ErrPeerAuthFailed: {
		Code: "E022", Exit: 22,
		Message: "Could not verify the other side.",
		Action: "Either the code you used doesn't match the one the sender shared,\n" +
			"  or someone on the network tried to interfere. Re-share the code\n" +
			"  and try again.",
	},
	ErrRelayCapHit: {
		Code: "E023", Exit: 23,
		Message: "The server's transfer-size limit was reached. Transfer aborted.",
		Action: "Only fallback transfers routed through the server count against this\n" +
			"  limit; same-network and direct internet transfers are uncapped.\n" +
			"  Run again from a different network so fsend can connect you directly." +
			selfHostHint,
	},
	ErrRelayBudgetExhausted: {
		Code: "E037", Exit: 37,
		Message: "The server hit its daily transfer budget. Transfer aborted.",
		Action: "Only fallback transfers routed through the server count against this\n" +
			"  budget; same-network and direct internet transfers don't.\n" +
			"  Run again from a different network so fsend can connect you directly,\n" +
			"  or try again after 00:00 UTC, when the budget resets." +
			selfHostHint,
	},
	ErrUsage: {
		Code: "E024", Exit: 24,
		Message: "Invalid usage.",
		Action:  "Run `fsend --help` for the full command surface.",
	},
	ErrSourceNotFound: {
		Code: "E025", Exit: 25,
		Message: "No such file or directory.",
	},
	ErrUserCancelled: {
		Code: "E026", Exit: 130,
		Message: "Cancelled.",
	},
	ErrLANListenerFailed: {
		Code: "E027", Exit: 27,
		Message: "Could not open the port fsend uses to find the other side on your local network.",
		Action: "Another fsend (or another program) may already be using that port.\n" +
			"  Try again — most codes pick a different port.",
	},
	ErrServerAuthRequired: {
		Code: "E028", Exit: 28,
		Message: "The server requires a password.",
		Action: "Set it with:\n" +
			"    fsend --connect <host:port>,<password>\n" +
			"  Ask the server operator for the password if you don't have it.",
	},
	ErrRelayIdleTimeout: {
		Code: "E029", Exit: 29,
		Message: "The server closed the connection because no data was flowing. Transfer aborted.",
		Action:  "The other side likely went away. Try again.",
	},
	ErrServerStartup: {
		Code: "E030", Exit: 30,
		Message: "The server could not start.",
		Action: "Fix the setting named above, or check that the ports are free and you\n" +
			"  have permission to bind them (FSEND_SERVER_ADDR, FSEND_RELAY_ADDR).\n" +
			"  Binding :443 may need elevated privileges.",
	},
	ErrPasswordRequired: {
		Code: "E031", Exit: 31,
		Message:       "This transfer requires a password.",
		Action:        "Rerun with --pass=<password> (or set FSEND_PASS).",
		SenderMessage: "The receiver couldn't supply the password.",
		SenderAction: "Run fsend again to issue a fresh code, and ask them to rerun with\n" +
			"  --pass=<password> (or FSEND_PASS).",
	},
	ErrPeerCancelled: {
		Code: "E032", Exit: 32,
		// Only the sender posts cancel notices today, so the base wording
		// is receiver-facing.
		Message: "The sender cancelled the transfer.",
		Action: "Ask the sender to run fsend again, then use the new code — the\n" +
			"  transfer will resume in this same directory.",
	},
	ErrUpdateFailed: {
		Code: "E033", Exit: 33,
		Message: "Could not update fsend.",
		Action: "Check your internet connection and try again, or reinstall:\n" +
			"    https://github.com/polius/fsend#install",
	},
	ErrIncompatibleVersion: {
		Code: "E034", Exit: 34,
		Message: "The other device is running an incompatible version of fsend.",
		Action: "Update both sides to the latest version, then try again:\n" +
			"    fsend --update",
	},
	ErrUninstallFailed: {
		Code: "E035", Exit: 35,
		Message: "fsend was not fully uninstalled.",
		Action: "Remove the binary by hand (the path is printed above), " +
			"possibly with sudo.",
	},
	ErrUnsendableSymlink: {
		Code: "E036", Exit: 36,
		Message: "Cannot send a symlink",
		Action:  "Fix the link, or skip it with --exclude.",
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
