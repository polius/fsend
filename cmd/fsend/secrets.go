package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/polius/fsend/internal/fserrors"
)

// passwordFromEnv resolves a password from the environment without ever
// touching argv. The plain variable wins when set; otherwise the
// Docker-secrets-style NAME_FILE variable is read from disk, so an operator
// can mount the secret instead of putting it in the process environment.
func passwordFromEnv(name string) (string, error) {
	if v := os.Getenv(name); v != "" {
		return v, nil
	}
	return readSecretFile(os.Getenv(name + "_FILE"))
}

// readSecretFile returns the contents of the file at path with trailing
// newlines stripped (a trailing newline is how editors and `docker secret`
// write files, and it must not become part of the password). An empty path
// means "not configured" and returns "".
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: reading secret file: %v", fserrors.ErrUsage, err)
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
