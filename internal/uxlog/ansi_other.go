//go:build !windows

package uxlog

// enableANSI reports whether the terminal can interpret ANSI escapes.
// Unix terminals always can.
func enableANSI() bool { return true }
