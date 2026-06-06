package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/term"

	"github.com/polius/fsend/internal/fserrors"
)

// readPasswordHidden prompts on stderr and reads a single line from the
// controlling terminal without echo. Falls back to plain stdin when
// stdin is not a TTY (e.g. inside a pipeline) — callers that need
// true no-echo should require the env var instead.
//
// Empty input gets one re-prompt with a hint, then surfaces as a usage
// error (mapped to E024) so the user sees a clean message instead of
// the E099 catchall.
func readPasswordHidden(prompt string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		pw, err := readPasswordOnce(prompt)
		if err != nil {
			// EOF on a piped stdin (or a closed terminal) means the
			// caller can't supply input — surface as a usage error,
			// not the E099 catchall.
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("%w: no password supplied (stdin closed)", fserrors.ErrUsage)
			}
			return "", err
		}
		if pw != "" {
			return pw, nil
		}
		if attempt == 0 {
			fmt.Fprintln(os.Stderr, "  Password cannot be empty — try again.")
		}
	}
	return "", fmt.Errorf("%w: password cannot be empty", fserrors.ErrUsage)
}

// readPasswordOnce performs a single hidden-input read. Returns "" with
// no error on empty input so the caller can re-prompt without conflating
// "empty" with "I/O failure".
func readPasswordOnce(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
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
	return line, nil
}
