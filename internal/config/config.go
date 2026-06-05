// Package config reads and writes the fsend CLI's user config file.
//
// Location follows XDG conventions:
//   - Linux:   $XDG_CONFIG_HOME/fsend/config.json (default ~/.config/fsend/...)
//   - macOS:   ~/Library/Application Support/fsend/config.json
//   - Windows: %AppData%\fsend\config.json
//
// The file is JSON for human-debuggability and is written with mode 0600
// because it may contain a relay shared password.
//
// Missing file or missing fields → silently fall back to compiled-in
// defaults. The CLI never errors out on a missing config file; that's the
// normal first-run state.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/adrg/xdg"

	"github.com/polius/fsend/internal/fserrors"
)

// SchemaVersion is the current config file schema version. We bump this when
// the on-disk format changes incompatibly; old files are treated as missing.
const SchemaVersion = 1

// Config is the on-disk shape of ~/.config/fsend/config.json.
//
// All fields are pointer/nullable where "unset" needs to be distinguished
// from "zero value" — for example a server set to the empty string is
// different from a server field that has never been written.
type Config struct {
	SchemaVersion  int    `json:"schema_version"`
	Server         string `json:"server,omitempty"`          // empty = use compiled-in default
	ServerPassword string `json:"server_password,omitempty"` // empty = no password
}

// DefaultServer is the compiled-in default rendezvous server.
//
// This value is only used when Config.Server is empty. Users can change
// the active server with `fsend --connect <host:port>` (writes Server) or
// revert with `fsend --connect default` (clears Server).
const DefaultServer = "fs.alzina.dev:443"

// EffectiveServer returns the server the CLI should actually use.
// Falls back to DefaultServer when the user has never customized.
func (c *Config) EffectiveServer() string {
	if c.Server == "" {
		return DefaultServer
	}
	return c.Server
}

// IsDefault reports whether the effective server is the compiled-in default.
func (c *Config) IsDefault() bool {
	return c.Server == ""
}

// pathOverride lets tests redirect config path resolution to a tempdir.
// In production this is always empty and the real XDG-based path is used.
var (
	pathMu       sync.RWMutex
	pathOverride string
)

// SetPathForTesting forces Path() to return the given path. Used only in tests.
func SetPathForTesting(p string) { pathMu.Lock(); pathOverride = p; pathMu.Unlock() }

// Path returns the absolute path where the config file lives.
//
// Production: resolved via XDG (Linux $XDG_CONFIG_HOME, macOS Application
// Support, Windows %AppData%).
// Test: overridden via SetPathForTesting.
//
// Errors only on truly broken environments where the home directory cannot
// be determined.
func Path() (string, error) {
	pathMu.RLock()
	override := pathOverride
	pathMu.RUnlock()
	if override != "" {
		return override, nil
	}
	rel := filepath.Join("fsend", "config.json")
	// xdg.ConfigFile creates parent dirs if they don't exist.
	p, err := xdg.ConfigFile(rel)
	if err != nil {
		return "", fmt.Errorf("resolving config path: %w", err)
	}
	return p, nil
}

// Load reads the config from disk. A missing file returns a zero-value
// Config and nil — that's the normal first-run state, not an error.
//
// An invalid file returns a zero-value Config and fserrors.ErrConfigCorrupted
// so the caller can warn the user but proceed with defaults.
func Load() (*Config, error) {
	p, err := Path()
	if err != nil {
		return &Config{}, nil // not fatal; fall back to defaults
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{}, nil
		}
		return &Config{}, nil // can't read for any reason → use defaults silently
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return &Config{}, fserrors.ErrConfigCorrupted
	}
	if c.SchemaVersion != SchemaVersion {
		// Future schema or junk; ignore.
		return &Config{}, fserrors.ErrConfigCorrupted
	}
	return &c, nil
}

// Save writes the config to disk atomically with mode 0600.
//
// Atomicity: write to a sibling tempfile, fsync, rename. Crash mid-write
// leaves the existing file untouched.
func Save(c *Config) error {
	if c == nil {
		return errors.New("config: cannot save nil")
	}
	c.SchemaVersion = SchemaVersion

	p, err := Path()
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating config dir: %w", err)
	}

	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	// Write to <path>.tmp then atomically rename.
	tmp, err := os.CreateTemp(dir, "config-*.tmp")
	if err != nil {
		return fmt.Errorf("creating tempfile: %w", err)
	}
	tmpName := tmp.Name()
	// Clean up tempfile on any error path.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing tempfile: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod tempfile: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("fsync tempfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing tempfile: %w", err)
	}
	if err := os.Rename(tmpName, p); err != nil {
		return fmt.Errorf("renaming into place: %w", err)
	}
	return nil
}
