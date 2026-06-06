package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"

	"golang.org/x/term"
)

// readPasswordHidden prompts on stderr and reads a single line from the
// controlling terminal without echo. Falls back to plain stdin when
// stdin is not a TTY (e.g. inside a pipeline) — callers that need
// true no-echo should require the env var instead.
func readPasswordHidden(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if len(b) == 0 {
			return "", errors.New("empty password")
		}
		return string(b), nil
	}
	// Non-TTY: read one line (no echo possible).
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	// Trim trailing newline / CRLF.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	if line == "" {
		return "", errors.New("empty password")
	}
	return line, nil
}
