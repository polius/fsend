package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

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

// resolvePassword expands the bare --pass sentinel into a concrete
// password. No-op when --pass wasn't given bare.
//
// Sender side: shows a freshly-generated 16-char suggestion the user can
// accept with <enter> (or override by typing one). The suggestion is
// echoed in cleartext — the sender needs to read it back to share with
// the receiver, and the input it shadows is shown in plaintext anyway.
//
// Receiver side: hidden no-echo prompt. The receiver must type whatever
// the sender configured, so a "random default" makes no sense; we just
// reuse the standard hidden-password reader.
func resolvePassword(f *flags, sender bool) error {
	if f.passArg != passPromptSentinel {
		return nil
	}
	if sender {
		pw, err := promptPasswordWithSuggestion()
		if err != nil {
			return err
		}
		f.passArg = pw
		return nil
	}
	pw, err := readPasswordHidden("Password for this transfer: ")
	if err != nil {
		return err
	}
	f.passArg = pw
	return nil
}

// promptPasswordWithSuggestion offers a freshly-generated random password
// as the default. The suggestion is printed visibly (the sender needs to
// see it to share it out-of-band) and a bare <enter> accepts it. Any
// non-empty typed input replaces the suggestion.
func promptPasswordWithSuggestion() (string, error) {
	suggested, err := generateRandomPassword(16)
	if err != nil {
		return "", fmt.Errorf("generating suggested password: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Suggested password: %s\n", suggested)
	fmt.Fprint(os.Stderr, "  Press Enter to use it, or type your own: ")

	line, err := stdinReader().ReadString('\n')
	if err != nil && line == "" {
		// EOF on a piped stdin with no data: the caller can't interact.
		// Be explicit rather than silently accepting the suggested
		// value — a script that pipes nothing into fsend would never
		// know what password was chosen.
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w: no password supplied (stdin closed)", fserrors.ErrUsage)
		}
		return "", err
	}
	typed := strings.TrimRight(line, "\r\n")
	typed = strings.TrimSpace(typed)
	if typed == "" {
		return suggested, nil
	}
	return typed, nil
}

// generateRandomPassword returns an n-character random string using a
// human-readable alphabet (no ambiguous characters like 0/O/1/l). Uses
// crypto/rand.Int for unbiased sampling — same pattern as
// internal/code.Generate.
func generateRandomPassword(n int) (string, error) {
	const alphabet = "ABCDEFGHJKMNPQRSTUVWXYZabcdefghjkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range b {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = alphabet[idx.Int64()]
	}
	return string(b), nil
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
	// Non-TTY: read one line (no echo possible). Use the package-level
	// shared bufio.Reader on os.Stdin so an earlier readLine() call
	// hasn't left buffered bytes in a now-discarded reader.
	line, err := stdinReader().ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	// Trim trailing newline / CRLF.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}
