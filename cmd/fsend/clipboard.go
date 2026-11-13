package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/polius/fsend/internal/uxlog"
)

// Moving the code to the other person is the one manual step in a
// transfer, so the sender puts "fsend <code>" on the clipboard: through the
// platform's helper when there is one (its exit status confirms the copy),
// else through an OSC 52 escape, which also works over SSH and inside tmux
// but reports nothing back. Only a confirmed copy is announced.

// clipboardTimeout bounds a clipboard helper: a hung X server must not hold
// up the code's appearance.
const clipboardTimeout = 2 * time.Second

// runClipboardHelper runs a helper with text on its stdin. A var so tests
// can stand in for the real helpers.
var runClipboardHelper = func(text string, argv []string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), clipboardTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is one of clipboardHelpers' fixed commands; only stdin carries data
	cmd.Stdin = bytes.NewBufferString(text)
	return cmd.Run() == nil
}

// lookPath is exec.LookPath, swappable in tests.
var lookPath = exec.LookPath

// clipboardHelpers lists the helpers to try, in order, for this platform and
// session. Over SSH none: a helper would fill the remote machine's
// clipboard, not the one in front of the user — OSC 52 reaches that one.
func clipboardHelpers() [][]string {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return nil
	}
	switch runtime.GOOS {
	case "darwin":
		return [][]string{{"pbcopy"}}
	case "windows":
		return [][]string{{"clip.exe"}}
	}
	var hs [][]string
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		hs = append(hs, []string{"wl-copy"})
	}
	if os.Getenv("DISPLAY") != "" {
		hs = append(hs, []string{"xclip", "-selection", "clipboard"}, []string{"xsel", "--clipboard", "--input"})
	}
	// WSL: the Windows clipboard, reachable from Linux.
	return append(hs, []string{"clip.exe"})
}

// clipboardDisabled reports the FSEND_NO_CLIPBOARD opt-out.
func clipboardDisabled() bool {
	v := os.Getenv("FSEND_NO_CLIPBOARD")
	return v != "" && v != "0" && v != "false"
}

// copyCommand puts text on the clipboard and reports whether a helper
// confirmed it. Only for an interactive terminal on stderr (the human
// block is showing), never for --password sends — those codes stay off the
// clipboard — and not when the user opted out.
func copyCommand(f *flags, text string) (confirmed bool) {
	if f.quiet || f.passArg != "" || clipboardDisabled() || !uxlog.CanEscape(os.Stderr) {
		return false
	}
	if copyViaHelper(text) {
		return true
	}
	// OSC 52: the terminal (or tmux, or the SSH client's terminal) sets
	// the clipboard. Unconfirmed, so nothing is announced.
	_, _ = os.Stderr.WriteString(osc52(text))
	return false
}

// copyViaHelper tries this platform's helpers in order; true when one ran
// and exited 0.
func copyViaHelper(text string) bool {
	for _, argv := range clipboardHelpers() {
		if _, err := lookPath(argv[0]); err != nil {
			continue
		}
		if runClipboardHelper(text, argv) {
			return true
		}
	}
	return false
}

// osc52 is the escape that asks the terminal to set its clipboard.
func osc52(text string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(text)) + "\a"
}
