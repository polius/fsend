//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// runInstaller re-runs the PowerShell installer pinned (via
// FSEND_PREFIX) to the directory binPath lives in. Windows locks a
// running .exe against overwrite but not rename, so the current image
// is moved aside first, restored if the installer fails, and the
// leftover .old is deleted once this process exits.
func runInstaller(binPath string) error {
	old := binPath + ".old"
	_ = os.Remove(old) // stale leftover from an interrupted update
	if err := os.Rename(binPath, old); err != nil {
		return fmt.Errorf("moving the running fsend.exe aside: %v", err)
	}

	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		"irm https://getfsend.alzina.dev/windows | iex")
	cmd.Env = append(os.Environ(), "FSEND_PREFIX="+filepath.Dir(binPath))
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		_ = os.Rename(old, binPath)
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
