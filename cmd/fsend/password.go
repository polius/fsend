package main

import (
	"context"
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

// stdinIsTTY reports whether stdin is an interactive terminal. A var (not
// a plain call) so it can be swapped out in tests without faking fds.
var stdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

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

// readPasswordHiddenCtx is readPasswordHidden that also aborts on ctx
// cancellation (Ctrl-C at the prompt), returning context.Canceled. The
// abandoned ReadPassword goroutine cannot run its own deferred terminal
// restore, so we capture the terminal state up front and restore it here.
func readPasswordHiddenCtx(ctx context.Context, prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	var oldState *term.State
	if term.IsTerminal(fd) {
		oldState, _ = term.GetState(fd)
	}
	type result struct {
		pw  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		pw, err := readPasswordHidden(prompt)
		ch <- result{pw, err}
	}()
	select {
	case <-ctx.Done():
		if oldState != nil {
			_ = term.Restore(fd, oldState)
			fmt.Fprintln(os.Stderr)
		}
		return "", context.Canceled
	case r := <-ch:
		return r.pw, r.err
	}
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
//
// ctx must come from signalContext(): once the handler is installed,
// SIGINT only cancels the context, so a prompt that doesn't select on
// ctx would leave Ctrl-C dead — and for the hidden prompt, the terminal
// stuck with echo off.
func resolvePassword(ctx context.Context, f *flags, sender bool) error {
	if f.passArg != passPromptSentinel {
		return nil
	}
	// Bare --pass needs a TTY: the sender prompt echoes a suggestion the
	// user accepts with Enter; the receiver prompt reads no-echo. Either
	// way, doing it on a piped stdin reads the file's first line as the
	// password and breaks the transfer. FSEND_PASS is the documented
	// non-interactive path.
	if !stdinIsTTY() {
		return fmt.Errorf("%w: bare --pass needs a terminal; use --pass <value> or FSEND_PASS", fserrors.ErrUsage)
	}
	if sender {
		pw, err := promptPasswordWithSuggestionCtx(ctx)
		if err != nil {
			return err
		}
		f.passArg = pw
		return nil
	}
	pw, err := readPasswordHiddenCtx(ctx, "Password for this transfer: ")
	if err != nil {
		return err
	}
	f.passArg = pw
	return nil
}

// promptPasswordWithSuggestionCtx is promptPasswordWithSuggestion that
// aborts on ctx cancellation (Ctrl-C at the prompt). The prompt echoes
// normally, so unlike the hidden reader there is no terminal state to
// restore — just move off the prompt line.
func promptPasswordWithSuggestionCtx(ctx context.Context) (string, error) {
	type result struct {
		pw  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		pw, err := promptPasswordWithSuggestion()
		ch <- result{pw, err}
	}()
	select {
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr)
		return "", context.Canceled
	case r := <-ch:
		return r.pw, r.err
	}
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
