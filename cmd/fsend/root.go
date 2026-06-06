package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/version"
)

// flags is the bag of CLI options shared across send/receive.
//
// Cobra-style: declared here, populated from cobra.Command flags.
type flags struct {
	// Mode override
	forceSend    bool
	forceReceive bool

	// Send-side
	textArg  string
	passArg  string // shared with receive-side: sender requires, receiver supplies
	hostname string
	excludes []string // glob patterns; applied when bundling a directory archive

	// Receive-side
	yes       bool
	outDir    string
	overwrite bool

	// Server selection — connectArgsRaw is the raw slice cobra hands
	// back; "no flag" vs "empty flag" is distinguished via Flags().Changed.
	connectArgsRaw []string

	// Misc
	quiet     bool
	debug     bool
	uninstall bool

	// First-positional args, recorded after parsing.
	posArgs []string
}

func rootCmd() *cobra.Command {
	f := &flags{}

	c := &cobra.Command{
		Use:   "fsend [file|dir|code|-]...",
		Short: "Peer-to-peer file transfer",
		Long: `fsend transfers files directly between two computers.

Examples:
  Send:      fsend report.pdf
  Receive:   fsend abc-defg-jkm
  Folder:    fsend ./myproject
  Stdin:     cat file | fsend -
  Text:      fsend --text "hello world"`,
		Args:               cobra.ArbitraryArgs,
		SilenceUsage:       true,
		SilenceErrors:      true,
		DisableSuggestions: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			f.posArgs = args
			return dispatch(cmd, f)
		},
	}

	c.SetVersionTemplate(version.String() + "\n")
	c.Version = version.Version

	// Transfer behavior
	c.Flags().StringVar(&f.textArg, "text", "", "send a literal string instead of a file")
	c.Flags().StringVar(&f.passArg, "pass", "",
		"password gate. Bare --pass prompts (no echo). Env: FSEND_PASS")
	// Bare --pass (no value) collapses to this sentinel; the dispatch
	// layer treats it as "ask interactively, with input hidden." We
	// blank DefValue so cobra's --help doesn't print the sentinel.
	passFlag := c.Flags().Lookup("pass")
	passFlag.NoOptDefVal = passPromptSentinel
	passFlag.DefValue = ""
	c.Flags().BoolVar(&f.yes, "yes", false, "auto-accept incoming transfers")
	c.Flags().StringVar(&f.outDir, "out", "", "receive into this directory")
	c.Flags().BoolVar(&f.overwrite, "overwrite", false, "overwrite existing files on receive")
	c.Flags().BoolVar(&f.quiet, "quiet", false, "suppress non-error output")
	c.Flags().StringVar(&f.hostname, "name", "", "override the hostname shown to the peer")
	c.Flags().StringSliceVar(&f.excludes, "exclude", nil,
		"glob patterns to skip when bundling a directory (repeatable or comma-separated)")

	// Mode override
	c.Flags().BoolVar(&f.forceSend, "send", false, "force send mode (skip auto-detect)")
	c.Flags().BoolVar(&f.forceReceive, "receive", false, "force receive mode (skip auto-detect)")

	// Server selection
	c.Flags().StringSliceVar(&f.connectArgsRaw, "connect", nil, "set the rendezvous server: <host:port> [password] | 'default'")

	// Misc
	c.Flags().BoolVar(&f.debug, "debug", false, "verbose logging to stderr")
	c.Flags().BoolVar(&f.uninstall, "uninstall", false, "remove the fsend binary and config dir")

	return c
}

// passPromptSentinel is the value cobra hands us when the user passes
// bare --pass with no argument. The dispatch layer translates this into
// an interactive no-echo prompt before any send/receive work begins.
//
// Note: a user who explicitly passes --pass=":prompt:" gets the same
// hidden prompt — surprising-but-fine, since they typed the literal
// sentinel themselves.
const passPromptSentinel = ":prompt:"

// dispatch implements the rules in PROJECT_SPEC.md "Dispatch rules".
func dispatch(cmd *cobra.Command, f *flags) error {
	// Handle --connect (server configuration) before anything else.
	if cmd.Flags().Changed("connect") {
		return runConnect(f)
	}

	// --uninstall is a maintenance command; never combine with transfer.
	if f.uninstall {
		return runUninstall(f)
	}

	// Env-var fallback for the password (FSEND_PASS). Passing a secret via
	// flag leaks it through /proc/<pid>/cmdline and `ps -ef`; the env var
	// lets users keep it out of argv. We only consult it when the flag
	// wasn't explicitly given, so scripts that set both keep the flag's
	// value (matching every other CLI's override convention).
	applyEnvFallbacks(f, cmd)

	// Bare --pass (no value) → ask the user once, hidden. Done here so
	// both send and receive paths benefit and the prompt happens before
	// any network activity.
	if f.passArg == passPromptSentinel {
		pw, err := readPasswordHidden("Enter password: ")
		if err != nil {
			return err
		}
		f.passArg = pw
	}

	// Force mode short-circuits auto-detect.
	if f.forceSend {
		return runSend(f, f.posArgs)
	}
	if f.forceReceive {
		if len(f.posArgs) != 1 {
			return errors.New("--receive requires exactly one positional argument (the code)")
		}
		return runReceive(f, f.posArgs[0])
	}

	// --text is unambiguously send mode.
	if f.textArg != "" {
		return runSend(f, nil)
	}

	// No positional args and no flag-driven send: print help.
	if len(f.posArgs) == 0 {
		return cmd.Help()
	}

	// "-" means send from stdin.
	if len(f.posArgs) == 1 && f.posArgs[0] == "-" {
		return runSend(f, []string{"-"})
	}

	// Multi-arg always means send (codes are single tokens).
	if len(f.posArgs) > 1 {
		return runSend(f, f.posArgs)
	}

	// Single arg: code-vs-path auto-detect.
	arg := f.posArgs[0]
	if code.IsCode(arg) {
		// Code regex match. If a file with that name exists in CWD, prompt.
		if _, err := os.Stat(arg); err == nil {
			return promptCodeOrPath(f, arg)
		}
		return runReceive(f, arg)
	}
	return runSend(f, []string{arg})
}

// applyEnvFallbacks fills in flags from environment variables when the
// user did not pass the corresponding flag. Used for secrets we'd
// rather not see on argv.
func applyEnvFallbacks(f *flags, cmd *cobra.Command) {
	if !cmd.Flags().Changed("pass") {
		if v := os.Getenv("FSEND_PASS"); v != "" {
			f.passArg = v
		}
	}
}

// promptCodeOrPath handles the rare collision case where a CLI arg both
// matches the code regex AND names a real file in CWD.
func promptCodeOrPath(f *flags, arg string) error {
	fmt.Fprintf(os.Stderr, "\n  %q matches a code AND is a local file.\n", arg)
	fmt.Fprintf(os.Stderr, "  [s]end this file, or [r]eceive with this code? ")

	var resp string
	if _, err := fmt.Fscanln(os.Stdin, &resp); err != nil {
		return fmt.Errorf("reading response: %w", err)
	}
	switch resp {
	case "s", "S", "send":
		return runSend(f, []string{arg})
	case "r", "R", "receive":
		return runReceive(f, arg)
	default:
		return errors.New("cancelled")
	}
}
