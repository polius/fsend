//go:build !windows

package main

// removeBinary is Windows-only; on every other platform runUninstall deletes
// the binary directly and never calls this. It exists so the windows branch
// in uninstall.go compiles cross-platform.
func removeBinary(string) error { return nil }
