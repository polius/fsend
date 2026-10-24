//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// installerAsset is the release asset runUpdate verifies and runs.
const installerAsset = "install.ps1"

// runInstaller runs the verified PowerShell installer pinned to version
// and to the directory binPath lives in (installerEnv). The installer
// moves the running image aside itself (a running .exe can be renamed,
// not overwritten) and swaps the fresh binary in; this function only
// reaps the leftover .old once this process exits and restores the
// original if the installer died between those two moves.
func runInstaller(binPath, script, version string) error {
	old := binPath + ".old"
	_ = os.Remove(old) // stale leftover from an interrupted update

	// Bypass is process-scoped: the default Restricted policy refuses any
	// -File script, and this one is already verified.
	cmd := exec.Command("powershell", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", script) //nolint:gosec // G204: the installer FetchInstaller just verified
	cmd.Env = installerEnv(binPath, version)
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
