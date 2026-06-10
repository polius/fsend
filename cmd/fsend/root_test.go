package main

import (
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

func TestBoldHelpHeaders(t *testing.T) {
	t.Run("colored", func(t *testing.T) {
		// FORCE_COLOR makes ColorFor true even though test stdout is
		// not a TTY, so the bolding path is exercised deterministically.
		t.Setenv("NO_COLOR", "")
		t.Setenv("FORCE_COLOR", "1")
		got := boldHelpHeaders(helpTemplate)
		for _, header := range []string{"USAGE", "EXAMPLES", "COMMON FLAGS",
			"ADVANCED FLAGS", "ENVIRONMENT", "SELF-HOSTING", "LEARN MORE"} {
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
