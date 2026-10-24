package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
			// No code, no FSEND_CODE, no terminal to ask on.
			"force_receive_zero_args",
			[]string{"--receive"},
			"--receive needs the code",
		},
		{
			"server_invalid",
			[]string{"--server=not a host!", "file.txt"},
			"--server: invalid server hostname",
		},
		{
			"connect_with_server",
			[]string{"--connect=host:443", "--server=other:443"},
			"--connect cannot be combined with --server",
		},
		{
			"as_on_file_send",
			[]string{"--as=x.sql", "--send", "file.txt"},
			"--as names a piped stream",
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
			// --check reports on --update; alone it would silently do
			// nothing and exit 0, which a CI job would read as "up to date".
			"check_without_update",
			[]string{"--check"},
			"--check reports on --update",
		},
		{
			"connect_with_check",
			[]string{"--connect=host:443", "--check"},
			"--connect cannot be combined with --check",
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

// The misplaced-password hint ("…if it's the password, use --password=…") is
// a guess for the ambiguous `fsend --password secret file` case. Under an
// explicit --send the positional is unambiguously a path, so the guess must
// be suppressed and the honest missing-file error stand.
func TestRootCmd_PasswordHintSkippedUnderSend(t *testing.T) {
	const hint = "if it's the password"

	// Without --send the hint fires (the trap this guard exists for).
	plain := rootCmd()
	plain.SetArgs([]string{"--password", "secret", "report.pdf"})
	plain.SetOut(io.Discard)
	plain.SetErr(io.Discard)
	if err := plain.Execute(); err == nil || !strings.Contains(err.Error(), hint) {
		t.Fatalf("plain --password: got %v, want the misplaced-password hint", err)
	}

	// With --send the positional is a path: no hint, and still an ErrUsage.
	forced := rootCmd()
	forced.SetArgs([]string{"--send", "--password", "secret", "report.pdf"})
	forced.SetOut(io.Discard)
	forced.SetErr(io.Discard)
	err := forced.Execute()
	if err == nil || !errors.Is(err, fserrors.ErrUsage) {
		t.Fatalf("--send --password: got %v, want an ErrUsage", err)
	}
	if strings.Contains(err.Error(), hint) {
		t.Errorf("--send must not guess the positional is a password: %q", err.Error())
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

// Dictated and chat-mangled codes receive (they reach the --quiet-without-
// --yes usage guard, which sits after code validation), unless a file by
// that exact name exists — real paths always win.
func TestDispatch_TolerantCodes(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, arg := range []string{"abc defg jkm", "abc_defg_jkm", "abcdefgjkm", "ABC-DEFG-JKM", "abc-defg-jkm.", " abc-defg-jkm\n"} {
		cmd := rootCmd()
		cmd.SetArgs([]string{"--quiet", arg})
		cmd.SetOut(io.Discard)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--quiet on receive requires --yes") {
			t.Errorf("fsend %q: got %v, want it received as a code", arg, err)
		}
	}
	// A real file named like a mangled code is sent, not received.
	if err := os.WriteFile("abcdefgjkm", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := rootCmd()
	cmd.SetArgs([]string{"--quiet", "--out=x", "abcdefgjkm"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--out is a receive-side flag") {
		t.Errorf("existing file: got %v, want the send path", err)
	}
}

// FSEND_CODE receives with no code in argv; bare --receive reads it too.
func TestDispatch_CodeFromEnv(t *testing.T) {
	t.Setenv("FSEND_CODE", "ABC DEFG JKM")
	for _, args := range [][]string{{"--quiet"}, {"--quiet", "--receive"}} {
		cmd := rootCmd()
		cmd.SetArgs(args)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "--quiet on receive requires --yes") {
			t.Errorf("%v with FSEND_CODE: got %v, want a receive", args, err)
		}
	}
}

// Stdin that isn't a data source (/dev/null in cron/CI, a closed fd)
// must not start an empty stdin send.
func TestDispatch_EmptyStdinShowsHelp(t *testing.T) {
	orig := stdinHasData
	stdinHasData = func() bool { return false }
	t.Cleanup(func() { stdinHasData = orig })
	origTTY := stdoutIsTTY
	stdoutIsTTY = func() bool { return false }
	t.Cleanup(func() { stdoutIsTTY = origTTY })

	var out strings.Builder
	cmd := rootCmd()
	cmd.SetArgs(nil)
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("bare fsend with empty stdin: %v", err)
	}
	if !strings.Contains(out.String(), "USAGE") {
		t.Errorf("want help, got %q", out.String())
	}
}

func TestStdinHasData(t *testing.T) {
	dir := t.TempDir()
	check := func(name string, f *os.File, want bool) {
		t.Helper()
		orig := os.Stdin
		os.Stdin = f
		defer func() { os.Stdin = orig }()
		if got := stdinHasData(); got != want {
			t.Errorf("%s: stdinHasData() = %v, want %v", name, got, want)
		}
	}
	empty, _ := os.Create(filepath.Join(dir, "empty"))
	defer empty.Close()
	check("empty file", empty, false)
	full := filepath.Join(dir, "full")
	_ = os.WriteFile(full, []byte("data"), 0o600)
	ff, _ := os.Open(full)
	defer ff.Close()
	check("non-empty file", ff, true)
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	check("pipe", r, true)
	if devnull, err := os.Open(os.DevNull); err == nil {
		defer devnull.Close()
		check("/dev/null", devnull, false)
	}
}

func TestHelpCommand(t *testing.T) {
	run := func(args ...string) (string, error) {
		var out strings.Builder
		cmd := rootCmd()
		cmd.SetArgs(args)
		cmd.SetOut(&out)
		err := cmd.Execute()
		return out.String(), err
	}
	if out, err := run("help", "advanced"); err != nil || !strings.Contains(out, "ENVIRONMENT") {
		t.Errorf("help advanced = %v, %q", err, out)
	}
	if _, err := run("help", "nope"); !errors.Is(err, fserrors.ErrUsage) {
		t.Errorf("unknown help topic: got %v, want E024", err)
	}
	// The everyday page stays short enough to scan.
	if n := strings.Count(helpTemplate, "\n"); n > 36 {
		t.Errorf("helpTemplate is %d lines; keep it to ~35 and move the rest to help advanced", n)
	}
}

// A typo'd flag names the flag it most likely meant.
func TestFlagSuggestion(t *testing.T) {
	cmd := rootCmd()
	cmd.SetArgs([]string{"--overwirte"})
	err := cmd.Execute()
	if !errors.Is(err, fserrors.ErrUsage) || !strings.Contains(err.Error(), "did you mean --overwrite?") {
		t.Errorf("got %v", err)
	}
	// Short flags and the old --name / curl-style --output spellings work.
	cmd = rootCmd()
	if err := cmd.ParseFlags([]string{"-y", "-q", "-o", "dir", "--name=box", "--output=d2"}); err != nil {
		t.Fatalf("short/alias flags: %v", err)
	}
	if v, _ := cmd.Flags().GetString("device-name"); v != "box" {
		t.Errorf("--name should set --device-name, got %q", v)
	}
	if v, _ := cmd.Flags().GetString("out"); v != "d2" {
		t.Errorf("--output should set --out, got %q", v)
	}
}

// Tab completion offers files for the positional (the file to send) and
// directories for --out.
func TestCompletion_CompletesFiles(t *testing.T) {
	var out strings.Builder
	cmd := rootCmd()
	cmd.SetArgs([]string{cobra.ShellCompRequestCmd, "rep"})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf(":%d\n", cobra.ShellCompDirectiveDefault)) {
		t.Errorf("positional completion directive: %q", out.String())
	}
	out.Reset()
	cmd = rootCmd()
	cmd.SetArgs([]string{cobra.ShellCompRequestCmd, "--out", ""})
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), fmt.Sprintf(":%d\n", cobra.ShellCompDirectiveFilterDirs)) {
		t.Errorf("--out completion directive: %q", out.String())
	}
}

func TestBoldHelpHeaders(t *testing.T) {
	t.Run("colored", func(t *testing.T) {
		// FORCE_COLOR makes ColorFor true even though test stdout is
		// not a TTY, so the bolding path is exercised deterministically.
		t.Setenv("NO_COLOR", "")
		t.Setenv("FORCE_COLOR", "1")
		got := boldHelpHeaders(helpTemplate)
		for _, header := range []string{"USAGE", "SENDING",
			"RECEIVING", "GENERAL", "LEARN MORE"} {
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
		if !strings.Contains(helpTemplate+advancedHelpTemplate, "--"+f.Name) {
			t.Errorf("flag --%s is registered but missing from the help pages", f.Name)
		}
		// Listed once on the everyday page, or the entries drift apart.
		if n := strings.Count(helpTemplate, "  --"+f.Name+" "); n > 1 {
			t.Errorf("flag --%s has %d entries in helpTemplate", f.Name, n)
		}
	})
}

// An explicit --receive must tolerate the same chat-app mangling
// (auto-capitalized first letter, wrapped whitespace) that auto-detect
// already fixes up — not fail E004 where the implicit path would work.
// The probe uses --quiet without --yes: a code that survives validation
// hits that usage guard (E024) before any network work, so E004-vs-E024
// distinguishes "rejected by format" from "normalized and accepted".
func TestStartReceive_NormalizesMangledCodes(t *testing.T) {
	for _, mangled := range []string{"Abc-Defg-Jkm", "ABC-DEFG-JKM", " abc-defg-jkm\n"} {
		err := startReceive(&flags{quiet: true}, mangled)
		if errors.Is(err, fserrors.ErrInvalidCodeFormat) {
			t.Errorf("startReceive(%q) rejected a normalizable code: %v", mangled, err)
		}
		if !errors.Is(err, fserrors.ErrUsage) {
			t.Errorf("startReceive(%q) = %v, want the quiet-without-yes usage guard", mangled, err)
		}
	}
	// Genuinely invalid stays E004.
	if err := startReceive(&flags{quiet: true}, "Not-A-Code-At-All"); !errors.Is(err, fserrors.ErrInvalidCodeFormat) {
		t.Errorf("invalid code should stay E004, got %v", err)
	}
}
