//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// runInstaller re-runs the PowerShell installer pinned (via
// FSEND_PREFIX) to the directory binPath lives in. The installer moves
// the running image aside itself (a running .exe can be renamed, not
// overwritten) and swaps the fresh binary in; this function only reaps
// the leftover .old once this process exits and restores the original
// if the installer died between those two moves.
func runInstaller(binPath string) error {
	old := binPath + ".old"
	_ = os.Remove(old) // stale leftover from an interrupted update

	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"irm https://getfsend.alzina.dev/windows | iex")
	// --update always wants the newest release: drop any FSEND_VERSION the
	// user exported, so a stale pin can't reinstall an older version over
	// a newer binary.
	env := slices.DeleteFunc(slices.Clone(os.Environ()), func(e string) bool {
		return strings.HasPrefix(e, "FSEND_VERSION=")
	})
	cmd.Env = append(env, "FSEND_VERSION=latest", "FSEND_PREFIX="+filepath.Dir(binPath))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		// No binary at binPath means the installer moved the running image
		// aside but never swapped the new one in: put the original back.
		if _, statErr := os.Stat(binPath); statErr != nil {
			_ = os.Rename(old, binPath)
		}
		return err
	}
	removeFileAfterExit(old)
	return nil
}

// removeFileAfterExit deletes path once this process exits and the
// image lock drops — same detached-PowerShell trick as removeBinary
// (uninstall_windows.go). Best-effort: a survivor is reaped by the
// next --update.
func removeFileAfterExit(path string) {
	script := fmt.Sprintf(
		`Wait-Process -Id %d -ErrorAction SilentlyContinue; `+
			`Start-Sleep -Milliseconds 300; `+
			`Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue`,
		os.Getpid(), psSingleQuote(path))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		// CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP: hidden, outlives us.
		CreationFlags: 0x08000000 | 0x00000200,
	}
	_ = cmd.Start()
}
