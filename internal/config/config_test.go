package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// withTempXDG points the config package at a tempdir for the duration of
// the test. Returns the file path the package will read from / write to.
func withTempXDG(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fsend", "config.json")
	SetPathForTesting(path)
	t.Cleanup(func() { SetPathForTesting("") })
	return path
}

func TestLoad_MissingFile_NoError(t *testing.T) {
	withTempXDG(t)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load on missing file should be nil error, got %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil zero Config")
	}
	if c.EffectiveServer() != DefaultServer {
		t.Errorf("EffectiveServer = %q, want default %q", c.EffectiveServer(), DefaultServer)
	}
	if !c.IsDefault() {
		t.Error("IsDefault should be true when no custom server set")
	}
}

func TestSaveLoad_Roundtrip(t *testing.T) {
	path := withTempXDG(t)

	original := &Config{
		Server:         "relay.example.com:443",
		ServerPassword: "secret",
	}
	if err := Save(original); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Confirm file mode is 0600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	// On Windows the mode bits won't be exactly 0600; only check on Unix.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode != 0o600 {
			// Soft check: at least, no world-read access.
			if mode&0o007 != 0 {
				t.Errorf("config file mode %o should not be world-readable", mode)
			}
		}
	}

	// Round-trip.
	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Server != original.Server {
		t.Errorf("Server: got %q, want %q", loaded.Server, original.Server)
	}
	if loaded.ServerPassword != original.ServerPassword {
		t.Errorf("ServerPassword mismatch")
	}
	if loaded.EffectiveServer() != "relay.example.com:443" {
		t.Errorf("EffectiveServer wrong: %q", loaded.EffectiveServer())
	}
	if loaded.IsDefault() {
		t.Error("IsDefault should be false when custom server set")
	}
}

func TestLoad_CorruptedJSON(t *testing.T) {
	path := withTempXDG(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load()
	if !errors.Is(err, fserrors.ErrConfigCorrupted) {
		t.Errorf("expected ErrConfigCorrupted, got %v", err)
	}
	if c == nil {
		t.Fatal("expected non-nil zero Config even on corruption")
	}
	if !c.IsDefault() {
		t.Error("corrupted config should fall back to defaults")
	}
}

func TestLoad_WrongSchemaVersion(t *testing.T) {
	path := withTempXDG(t)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"schema_version": 999, "server": "foo:443"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load()
	if !errors.Is(err, fserrors.ErrConfigCorrupted) {
		t.Errorf("expected ErrConfigCorrupted for future schema, got %v", err)
	}
}

func TestSave_AtomicityWithCrashSimulation(t *testing.T) {
	// We can't truly simulate a crash, but we can confirm that after a
	// successful save, no tempfiles are left lying around.
	path := withTempXDG(t)

	if err := Save(&Config{Server: "first.example.com:443"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := Save(&Config{Server: "second.example.com:443"}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if name != "config.json" {
			t.Errorf("unexpected file in config dir after save: %q", name)
		}
	}
}

func TestSave_NilConfig(t *testing.T) {
	withTempXDG(t)
	if err := Save(nil); err == nil {
		t.Error("Save(nil) should error")
	}
}
