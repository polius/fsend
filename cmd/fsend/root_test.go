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
