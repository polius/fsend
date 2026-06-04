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
	codeArg    string
	textArg    string
	noCompress bool
	noClip     bool
	password   string
	hostname   string

	// Receive-side
	yes       bool
	outDir    string
	overwrite bool

	// Server selection
	connectArg    string
	connectGiven  bool
	connectArgsRaw []string

	// Misc
	quiet         bool
	noUpdateCheck bool
	debug         bool

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
  Receive:   fsend abc-defgh-jkm
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
	c.Flags().StringVar(&f.codeArg, "code", "", "use a specific code (implies send mode)")
	c.Flags().StringVar(&f.textArg, "text", "", "send a literal string instead of a file")
	c.Flags().BoolVar(&f.noCompress, "no-compress", false, "force-disable compression")
	c.Flags().BoolVar(&f.yes, "yes", false, "auto-accept incoming transfers")
	c.Flags().StringVar(&f.outDir, "out", "", "receive into this directory")
	c.Flags().BoolVar(&f.overwrite, "overwrite", false, "overwrite existing files on receive")
	c.Flags().BoolVar(&f.quiet, "quiet", false, "suppress non-error output")
	c.Flags().BoolVar(&f.noClip, "no-clipboard", false, "don't auto-copy the code to the clipboard")
	c.Flags().StringVar(&f.password, "pass", "", "require the receiver to enter this password")
	c.Flags().StringVar(&f.hostname, "name", "", "override the hostname shown to the peer")

	// Mode override
	c.Flags().BoolVar(&f.forceSend, "send", false, "force send mode (skip auto-detect)")
	c.Flags().BoolVar(&f.forceReceive, "receive", false, "force receive mode (skip auto-detect)")

	// Server selection
	c.Flags().StringSliceVar(&f.connectArgsRaw, "connect", nil, "set the rendezvous server: <host:port> [password] | 'default'")
	// connectGiven tells us whether the flag was passed at all (vs default
	// "no value"); cobra represents an empty slice and an unset slice
	// identically, so we look at Changed() in dispatch.

	// Misc
	c.Flags().BoolVar(&f.noUpdateCheck, "no-update-check", false, "skip GitHub release check")
	c.Flags().BoolVar(&f.debug, "debug", false, "verbose logging to stderr")

	return c
}

// dispatch implements the rules in PROJECT_SPEC.md "Dispatch rules".
func dispatch(cmd *cobra.Command, f *flags) error {
	// Handle --connect (server configuration) before anything else.
	if cmd.Flags().Changed("connect") {
		return runConnect(f)
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

	// --code is unambiguously send mode.
	if f.codeArg != "" {
		return runSend(f, f.posArgs)
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
