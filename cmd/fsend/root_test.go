package main

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/pflag"

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
			// Regression: `--password=` skipped every password path, so the
			// user believed the transfer was gated when it wasn't.
			"pass_empty_value",
			[]string{"--password="},
			"--password requires a non-empty password",
		},
		{
			// A bare --password followed by a non-file, non-code word is very
			// likely a misplaced inline password — point at the = form.
			"pass_bare_then_nonfile",
			[]string{"--password", "secret", "report.pdf"},
			"if it's the password, use --password=secret",
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
		{
			"connect_with_update",
			[]string{"--connect=host:443", "--update"},
			"--connect cannot be combined with --update",
		},
		{
			// Regression: --checksum/--manifest/--preview were missing from
			// the conflict list, so --connect persisted the server (a durable
			// global mutation) while silently dropping them.
			"connect_with_checksum",
			[]string{"--connect=host:443", "--checksum"},
			"--connect cannot be combined with --checksum",
		},
		{
			"connect_with_manifest",
			[]string{"--connect=host:443", "--manifest=m.csv"},
			"--connect cannot be combined with --manifest",
		},
		{
			"connect_with_preview",
			[]string{"--connect=host:443", "--preview"},
			"--connect cannot be combined with --preview",
		},
		{
			"update_with_uninstall",
			[]string{"--update", "--uninstall"},
			"--update and --uninstall are mutually exclusive",
		},
		{
			// Regression: `fsend report.pdf --update` ran the updater and
			// silently dropped the file the user asked to send.
			"update_with_positional",
			[]string{"report.pdf", "--update"},
			"--update cannot be combined with positional arguments",
		},
		{
			"update_with_yes",
			[]string{"--update", "--yes"},
			"--update cannot be combined with --yes",
		},
		{
			// Worst case of the same hole: `--uninstall --yes` in a mangled
			// script deleted the binary when a transfer was intended.
			"uninstall_with_positional",
			[]string{"somefile", "--uninstall"},
			"--uninstall cannot be combined with positional arguments",
		},
		{
			"uninstall_with_out",
			[]string{"--uninstall", "--out=/tmp"},
			"--uninstall cannot be combined with --out",
		},
		{
			// --preview is send-side; rejected when the arg is a code.
			"preview_on_receive",
			[]string{"--preview", "abc-defg-jkm"},
			"--preview is a send-side flag",
		},
		{
			// --manifest is receive-side; rejected when sending.
			"manifest_on_send",
			[]string{"--manifest=m.csv", "file.txt"},
			"--manifest is a receive-side flag",
		},
		{
			// --manifest records files on disk; --out - writes none.
			"manifest_with_stdout",
			[]string{"--manifest=m.csv", "--out", "-", "abc-defg-jkm"},
			"--manifest has no effect with --out -",
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

// --uninstall --yes is documented (skip the confirmation), so the guard
// must exempt it — unlike --update, where --yes answers nothing.
func TestMaintenanceGuard_AllowsYesForUninstall(t *testing.T) {
	cmd := rootCmd()
	if err := cmd.ParseFlags([]string{"--uninstall", "--yes"}); err != nil {
		t.Fatal(err)
	}
	if err := maintenanceGuard(cmd, &flags{}, "--uninstall", true); err != nil {
		t.Fatalf("--uninstall --yes must be allowed, got %v", err)
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

// wrappedCode detects a share code surrounded by whitespace (redirected
// newline, pasted space, CRLF) so it's received rather than mistaken for a
// filename — while leaving clean codes, malformed codes, and filenames alone.
func TestWrappedCode(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{"abc-defg-hjk", "", false},                // clean → handled by normal path
		{"abc-defg-hjk\n", "abc-defg-hjk", true},   // redirected newline
		{"abc-defg-hjk ", "abc-defg-hjk", true},    // trailing space
		{" abc-defg-hjk", "abc-defg-hjk", true},    // leading space
		{"abc-defg-hjk\r\n", "abc-defg-hjk", true}, // Windows CRLF
		{"\nabc-defg-hjk\n", "abc-defg-hjk", true}, // surrounded
		{"Abc-defg-hjk\n", "abc-defg-hjk", true},   // chat-app: capitalized + newline
		{"ABC-DEFG-HJK ", "abc-defg-hjk", true},    // uppercase + space
		{"abc - defg - hjk", "", false},            // internal spaces → still malformed
		{"report.pdf ", "", false},                 // filename with a space → not a code
	} {
		got, ok := wrappedCode(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("wrappedCode(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestBoldHelpHeaders(t *testing.T) {
	t.Run("colored", func(t *testing.T) {
		// FORCE_COLOR makes ColorFor true even though test stdout is
		// not a TTY, so the bolding path is exercised deterministically.
		t.Setenv("NO_COLOR", "")
		t.Setenv("FORCE_COLOR", "1")
		got := boldHelpHeaders(helpTemplate)
		for _, header := range []string{"USAGE", "EXAMPLES", "SENDING",
			"RECEIVING", "GENERAL", "ADVANCED", "LEARN MORE"} {
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

// TestHelpTemplate_ListsEveryFlag guards against the hand-written help
// drifting out of sync with the registered flags — every non-hidden flag
// must appear in helpTemplate. (--password once had two spellings here.)
func TestHelpTemplate_ListsEveryFlag(t *testing.T) {
	rootCmd().Flags().VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		if !strings.Contains(helpTemplate, "--"+f.Name) {
			t.Errorf("flag --%s is registered but missing from helpTemplate", f.Name)
		}
	})
}
