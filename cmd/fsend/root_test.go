package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// dispatch returns ErrUsage with a specific message for each invalid
// flag combination. Lock the contract in so refactors can't silently
// drop a guard or change the wording users will see in [E024].
func TestRootCmd_RejectsInvalidFlagCombinations(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{
			"connect_with_send",
			[]string{"--connect=host:443", "--send", "file.txt"},
			"--connect cannot be combined with --send",
		},
		{
			"connect_with_text",
			[]string{"--connect=host:443", "--text=hi"},
			"--connect cannot be combined with --text",
		},
		{
			"send_and_receive_both",
			[]string{"--send", "--receive", "abc-defg-jkm"},
			"--send and --receive are mutually exclusive",
		},
		{
			"invalid_mode",
			[]string{"--mode=garbage"},
			`invalid --mode "garbage"`,
		},
		{
			"force_receive_zero_args",
			[]string{"--receive"},
			"--receive requires exactly one positional argument",
		},
		{
			"force_receive_two_args",
			[]string{"--receive", "abc-defg-jkm", "extra"},
			"--receive requires exactly one positional argument",
		},
		{
			"text_empty_string",
			[]string{"--text="},
			"--text requires a non-empty string",
		},
		{
			// Regression: previously `fsend --connect host:port file.pdf`
			// silently glued file.pdf onto --connect and saved it as the
			// server password.
			"connect_with_positional",
			[]string{"--connect=host:443", "file.pdf"},
			"--connect cannot be combined with positional arguments",
		},
		{
			"connect_with_exclude",
			[]string{"--connect=host:443", "--exclude=*.tmp"},
			"--connect cannot be combined with --exclude",
		},
		{
			// `--connect default,<pw>` splits to ["default","<pw>"]; the
			// stray password must error, not be silently dropped.
			"connect_default_with_password",
			[]string{"--connect=default,hunter2"},
			"--connect default takes no password",
		},
		{
			// Regression: `--pass=` skipped every password path, so the
			// user believed the transfer was gated when it wasn't.
			"pass_empty_value",
			[]string{"--pass="},
			"--pass requires a non-empty password",
		},
		{
			// Regression: a code-shaped --pass value with piped stdin
			// silently opened a send session, code-as-password. (Space
			// form `--pass <code>` is unaffected: NoOptDefVal keeps the
			// code positional.)
			"pass_code_value",
			[]string{"--pass=abc-defg-jkm"},
			"to receive with a password: fsend abc-defg-jkm --pass",
		},
		{
			// LooksLikeCode near-miss (digit 1 for letter) gets the same hint.
			"pass_mistyped_code_value",
			[]string{"--pass=abc-defg-jk1"},
			"to receive with a password: fsend abc-defg-jk1 --pass",
		},
		{
			// Same trap on --connect: the code would be persisted as the
			// server host.
			"connect_code_value",
			[]string{"--connect=abc-defg-jkm"},
			"to receive: fsend abc-defg-jkm",
		},
		{
			"exclude_on_receive",
			[]string{"--receive", "abc-defg-jkm", "--exclude=*.log"},
			"--exclude only applies when sending a directory",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			cmd := rootCmd()
			cmd.SetArgs(c.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !errors.Is(err, fserrors.ErrUsage) {
				t.Errorf("got %v, want wrapping ErrUsage", err)
			}
			if !strings.Contains(err.Error(), c.wantSub) {
				t.Errorf("got %q, want substring %q", err.Error(), c.wantSub)
			}
		})
	}
}

// askCodeOrPath must not consent on EOF and must re-prompt on a typo
// instead of hard-erroring (same policy as promptAccept).
func TestAskCodeOrPath(t *testing.T) {
	ask := func(t *testing.T, in string) (send bool, stderr string, err error) {
		t.Helper()
		stderr = captureStderr(t, func() {
			send, err = askCodeOrPath(bufio.NewReader(strings.NewReader(in)), "abc-defg-jkm")
		})
		return
	}

	t.Run("eof_aborts", func(t *testing.T) {
		_, _, err := ask(t, "")
		if !errors.Is(err, fserrors.ErrUsage) {
			t.Fatalf("EOF must abort with ErrUsage, got %v", err)
		}
		if !strings.Contains(err.Error(), "--receive") {
			t.Errorf("error should point at --receive/--send: %v", err)
		}
	})

	t.Run("enter_defaults_to_receive", func(t *testing.T) {
		send, _, err := ask(t, "\n")
		if err != nil || send {
			t.Errorf("bare Enter = (%v, %v), want receive", send, err)
		}
	})

	t.Run("send_answer", func(t *testing.T) {
		send, _, err := ask(t, "s\n")
		if err != nil || !send {
			t.Errorf("'s' = (%v, %v), want send", send, err)
		}
	})

	t.Run("typo_reprompts", func(t *testing.T) {
		send, stderr, err := ask(t, "x\nr\n")
		if err != nil || send {
			t.Errorf("typo then 'r' = (%v, %v), want receive", send, err)
		}
		if !strings.Contains(stderr, "Please answer s or r") {
			t.Errorf("missing re-prompt hint:\n%s", stderr)
		}
	})
}

func TestBoldHelpHeaders(t *testing.T) {
	t.Run("colored", func(t *testing.T) {
		// FORCE_COLOR makes ColorFor true even though test stdout is
		// not a TTY, so the bolding path is exercised deterministically.
		t.Setenv("NO_COLOR", "")
		t.Setenv("FORCE_COLOR", "1")
		got := boldHelpHeaders(helpTemplate)
		for _, header := range []string{"USAGE", "EXAMPLES", "COMMON FLAGS",
			"ADVANCED FLAGS", "LEARN MORE"} {
			if !strings.Contains(got, "\x1b[1m"+header+"\x1b[0m") {
				t.Errorf("header %q not bolded", header)
			}
		}
		// Indented body lines and the lowercase version header line must
		// pass through untouched.
		if !strings.Contains(got, "\n  fsend <file|dir>...") {
			t.Error("indented body line was altered")
		}
		if strings.Contains(got, "\x1b[1mfsend —") {
			t.Error("version header line should not be bolded")
		}
	})

	t.Run("no_color_untouched", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		if got := boldHelpHeaders(helpTemplate); got != helpTemplate {
			t.Error("template must be byte-for-byte unchanged when color is off")
		}
	})
}
