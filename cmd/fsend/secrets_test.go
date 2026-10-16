package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPasswordFromEnv covers B6's secret-file support and its precedence:
// the plain env var wins, NAME_FILE is the fallback, a trailing newline is
// trimmed, and an unreadable file is a hard error rather than a silent
// "no password".
func TestPasswordFromEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("FSEND_PASSWORD", "")
	t.Setenv("FSEND_PASSWORD_FILE", path)
	if got, err := passwordFromEnv("FSEND_PASSWORD"); err != nil || got != "file-secret" {
		t.Errorf("file fallback = %q, err=%v, want file-secret", got, err)
	}

	t.Setenv("FSEND_PASSWORD", "env-secret")
	if got, err := passwordFromEnv("FSEND_PASSWORD"); err != nil || got != "env-secret" {
		t.Errorf("plain env should win: got %q, err=%v", got, err)
	}

	t.Setenv("FSEND_PASSWORD", "")
	t.Setenv("FSEND_PASSWORD_FILE", filepath.Join(dir, "missing"))
	if _, err := passwordFromEnv("FSEND_PASSWORD"); err == nil {
		t.Error("unreadable secret file should error, not silently degrade")
	}

	t.Setenv("FSEND_PASSWORD_FILE", "")
	if got, err := passwordFromEnv("FSEND_PASSWORD"); err != nil || got != "" {
		t.Errorf("unset = %q, err=%v, want empty", got, err)
	}
}

// TestApplyEnvFallbacks_PasswordFile confirms the CLI actually consumes the
// file form when no --password flag is given.
func TestApplyEnvFallbacks_PasswordFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pw")
	if err := os.WriteFile(path, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FSEND_PASSWORD", "")
	t.Setenv("FSEND_PASSWORD_FILE", path)

	c := rootCmd()
	f := &flags{}
	if err := applyEnvFallbacks(f, c); err != nil {
		t.Fatal(err)
	}
	if f.passArg != "from-file" {
		t.Errorf("passArg = %q, want from-file", f.passArg)
	}
}
