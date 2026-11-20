//go:build !(darwin || freebsd || openbsd || netbsd || dragonfly || linux)

package uxlog

// StopFlowControl is a no-op where terminals have no IXON (Windows).
func StopFlowControl() {}

// RestoreFlowControl is a no-op where StopFlowControl is.
func RestoreFlowControl() {}
