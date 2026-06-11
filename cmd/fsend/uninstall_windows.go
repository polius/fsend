//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// removeBinary deletes the running fsend.exe and (if it ends up empty) its
// install dir, and strips that dir from the user PATH.
//
// A running .exe can't delete its own image on Windows, so the delete is done
// by a detached cmd that retries until this process exits and releases the
// lock. The PATH edit happens here, synchronously, since it doesn't depend on
// the binary being gone.
func removeBinary(binPath string) error {
	dir := filepath.Dir(binPath)
	removeUserPathEntry(dir)

	// `del` clears fsend.exe; the loop's leading ping gives us time to exit so
	// the lock is released. Plain `rmdir` (no /s) only removes the dir if it's
	// now empty, leaving any unrelated files the user put there untouched.
	arg := fmt.Sprintf(
		`for /L %%i in (1,1,10) do (ping 127.0.0.1 -n 2 >nul & del /f /q "%s" 2>nul) & rmdir "%s" 2>nul`,
		binPath, dir)
	cmd := exec.Command("cmd", "/C", arg)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		// DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP: outlive us, no console.
		CreationFlags: 0x00000008 | 0x00000200,
	}
	return cmd.Start()
}

// removeUserPathEntry drops dir from the HKCU user PATH. PowerShell is always
// present on Windows, so we shell out to it rather than pull in a registry
// dependency. Best-effort: a failure just leaves a harmless stale PATH entry.
func removeUserPathEntry(dir string) {
	ps := fmt.Sprintf(
		`$d=%s; $p=[Environment]::GetEnvironmentVariable('Path','User'); `+
			`if($p){[Environment]::SetEnvironmentVariable('Path', `+
			`(($p -split ';' | Where-Object { $_ -and $_ -ne $d }) -join ';'), 'User')}`,
		psSingleQuote(dir))
	_ = exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps).Run()
}

// psSingleQuote wraps s in a PowerShell single-quoted string (only ' needs
// escaping, by doubling — backslashes are literal inside single quotes).
func psSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
