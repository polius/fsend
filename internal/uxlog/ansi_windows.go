//go:build windows

package uxlog

import (
	"os"

	"golang.org/x/sys/windows"
)

// enableANSI switches the Windows console into VT mode so ANSI escapes
// (color, cursor movement) render instead of printing as garbage.
// Returns false when any console handle refuses (legacy conhost), in
// which case every renderer falls back to its non-TTY output.
func enableANSI() bool {
	ok := true
	for _, f := range []*os.File{os.Stderr, os.Stdout} {
		h := windows.Handle(f.Fd())
		var mode uint32
		if windows.GetConsoleMode(h, &mode) != nil {
			continue // not a console (pipe/file) — nothing to enable
		}
		if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
			continue
		}
		if windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
			ok = false
		}
	}
	return ok
}
