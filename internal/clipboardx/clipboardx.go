// Package clipboardx is a thin wrapper around atotto/clipboard that
// degrades gracefully when no clipboard backend is available.
//
// PROJECT_SPEC.md design rule 3 says "Clipboard copy on by default", with
// `--no-clipboard` opting out, AND that the "✓ Code copied to clipboard"
// line is visible so users don't double-copy. The wrapper centralizes the
// "did it actually copy?" decision so callers can render the message
// truthfully.
//
// On a fresh Linux box without xclip/xsel installed, atotto/clipboard
// returns an error from WriteAll. We surface that as Ok=false rather than
// bubbling up — clipboard failure is never fatal.
package clipboardx

import "github.com/atotto/clipboard"

// Copy writes s to the user's clipboard.
//
// Returns true on success, false on any error (no clipboard backend,
// permission denied, etc.). Callers should render the "copied to
// clipboard" confirmation only when this returns true.
func Copy(s string) bool {
	if clipboard.Unsupported {
		return false
	}
	if err := clipboard.WriteAll(s); err != nil {
		return false
	}
	return true
}
