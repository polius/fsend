//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// removeBinary deletes the running fsend.exe and (if it ends up empty) its
// install dir, and strips that dir from the user PATH.
//
// A running .exe can't delete its own image on Windows, so the delete is done
// by a detached PowerShell that Wait-Process'es on our PID, then removes the
// file the instant we exit and the lock drops. PowerShell (not cmd) with
// single-quoted literal paths keeps double quotes out of the command line,
// avoiding the cmd.exe/CreateProcess escaping pitfalls that mangle paths.
// The PATH edit happens here, synchronously — it doesn't need the exe gone.
func removeBinary(binPath string) error {
	dir := filepath.Dir(binPath)
	removeUserPathEntry(dir)

	// Remove-Item on the dir without -Recurse only deletes it if now empty,
	// leaving any unrelated files the user put there untouched.
	script := fmt.Sprintf(
		`Wait-Process -Id %d -ErrorAction SilentlyContinue; `+
			`Start-Sleep -Milliseconds 300; `+
			`Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue; `+
			`Remove-Item -LiteralPath %s -Force -ErrorAction SilentlyContinue`,
		os.Getpid(), psSingleQuote(binPath), psSingleQuote(dir))
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true,
		// CREATE_NO_WINDOW | CREATE_NEW_PROCESS_GROUP: hidden, outlives us.
		CreationFlags: 0x08000000 | 0x00000200,
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
