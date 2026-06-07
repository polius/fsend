package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/fserrors"
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

	// Debug-only sender path override: "local" | "stun" | "turn".
	// Forces a specific data path instead of the default LAN+pairing-server
	// race with ICE-then-relay fallback. Hidden from --help, undocumented.
	mode string

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
		Use:   "fsend [file|dir|code]...",
		Short: "Peer-to-peer file transfer",
		Long: `fsend transfers files directly between two computers.

Examples:
  Send:      fsend report.pdf
  Receive:   fsend abc-defg-jkm
  Folder:    fsend ./myproject
  Stdin:     cat file | fsend
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

	// Route cobra's own flag-parse errors ("flag needs an argument",
	// "unknown flag", ...) through ErrUsage so they render with the
	// catalog [E024] line instead of dropping into the E099 "please
	// file an issue" catchall.
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return fmt.Errorf("%w: %v", fserrors.ErrUsage, err)
	})

	c.SetVersionTemplate(version.String() + "\n")
	c.Version = version.Version

	// Hand-written help/usage layout, replacing cobra's alphabetised
	// flag wall. Examples-first; common flags grouped semantically;
	// advanced flags last; environment variables called out explicitly.
	//
	// The header carries the build version so users see what they're
	// running on every `--help` — saves a round-trip to `--version`
	// when writing a bug report.
	ht := strings.Replace(helpTemplate, "fsend —", "fsend "+version.Version+" —", 1)
	c.SetHelpTemplate(ht)
	c.SetUsageTemplate(ht)

	// Transfer behavior
	c.Flags().StringVar(&f.textArg, "text", "", "send a literal string instead of a file")
	c.Flags().StringVar(&f.passArg, "pass", "",
		"password gate. Bare --pass prompts (sender: suggests a random default). Env: FSEND_PASS")
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

	// Server selection. Bare --connect (no value) means "show current
	// server" — same NoOptDefVal trick as --pass. The dispatcher
	// recognises the sentinel and treats it as "no args".
	c.Flags().StringSliceVar(&f.connectArgsRaw, "connect", nil, "set the server: <host:port> [password] | 'default'")
	connectFlag := c.Flags().Lookup("connect")
	connectFlag.NoOptDefVal = connectShowSentinel
	connectFlag.DefValue = ""

	// Misc
	c.Flags().BoolVar(&f.debug, "debug", false, "verbose logging to stderr")
	c.Flags().BoolVar(&f.uninstall, "uninstall", false, "remove the fsend binary and config dir")

	c.Flags().StringVar(&f.mode, "mode", "", "")
	_ = c.Flags().MarkHidden("mode")

	c.AddCommand(serverCmd())

	return c
}

// validMode reports whether s is an accepted --mode value.
// "" means "no override" (default auto-selection).
func validMode(s string) bool {
	switch s {
	case "", modeLocal, modeSTUN, modeTURN:
		return true
	}
	return false
}

const (
	modeLocal = "local"
	modeSTUN  = "stun"
	modeTURN  = "turn"
)

// helpTemplate is the single help/usage view. cobra invokes it for both
// `fsend --help` and `fsend` (the no-args path that prints help). The
// layout deliberately omits cobra's auto-generated flag wall — every
// user-facing flag is listed by hand below so we control wording and
// grouping.
const helpTemplate = `fsend — peer-to-peer file transfer

USAGE
  fsend <file|dir>...              Send (one or more paths)
  fsend <code>                     Receive (using a code like abc-defg-jkm)
  cat file.txt | fsend             Send from a pipe (or: fsend < file)
  fsend --text "hello world"       Send a literal string

EXAMPLES
  Send a file:
    fsend report.pdf
  Receive:
    fsend abc-defg-jkm
  Send a whole folder:
    fsend ./myproject
  Send with extra password protection:
    fsend report.pdf --pass "shared-secret"
  Use a different server:
    fsend --connect relay.mycompany.com:443

COMMON FLAGS
  --text "<string>"      Send a literal string instead of a file
  --pass <password>      Require the receiver to enter a password.
                         Bare --pass prompts interactively — sender side
                         suggests a fresh random default (press Enter to
                         accept). Env: FSEND_PASS.
  --out <dir>            Receive into this directory (default: current)
  --yes                  Auto-accept incoming transfers (no prompt)
  --overwrite            Overwrite existing files on receive
  --quiet                Suppress all non-error output
  --name <string>        Override the hostname shown to the peer
  --exclude <glob,…>     Skip entries matching these globs in a directory
  --help                 Show this help
  --version              Show version

ADVANCED FLAGS
  --connect              Show current server
  --connect <host:port> [password]
                         Set the server (persisted)
  --connect default      Revert to the compiled-in default server
  --send / --receive     Force mode (skip code/path auto-detect)
  --debug                Verbose logging to stderr (also: FSEND_DEBUG=1)
  --uninstall            Remove the fsend binary and its config dir

ENVIRONMENT
  FSEND_PASS             Used as --pass when the flag is not given
                         (keeps the password out of argv)

SELF-HOSTING
  fsend server                 Run your own pairing + relay server
                               (env-var config — see "fsend server --help")

LEARN MORE
  Docs    https://github.com/polius/fsend
  Issues  https://github.com/polius/fsend/issues
`

// passPromptSentinel is the value cobra hands us when the user passes
// bare --pass with no argument. The dispatch layer translates this into
// an interactive no-echo prompt before any send/receive work begins.
//
// Note: a user who explicitly passes --pass=":prompt:" gets the same
// hidden prompt — surprising-but-fine, since they typed the literal
// sentinel themselves.
const passPromptSentinel = ":prompt:"

// connectShowSentinel is the value cobra hands us when the user passes
// bare --connect with no argument. Runs the "show current server" path.
// Same caveat as passPromptSentinel: a literal `--connect ":show:"` is
// indistinguishable, but typing the sentinel by hand is squarely on the
// user.
const connectShowSentinel = ":show:"

// dispatch implements the CLI dispatch rules documented on the main.go
// package comment.
func dispatch(cmd *cobra.Command, f *flags) error {
	// Handle --connect (server configuration) before anything else.
	// Reject pairings with transfer-mode flags so the user can't be
	// surprised by "I asked for both --connect and --send and only the
	// first ran."
	if cmd.Flags().Changed("connect") {
		for _, conflict := range []string{"send", "receive", "text", "pass", "yes", "out", "overwrite", "name", "uninstall"} {
			if cmd.Flags().Changed(conflict) {
				return fmt.Errorf("%w: --connect cannot be combined with --%s", fserrors.ErrUsage, conflict)
			}
		}
		return runConnect(f)
	}

	// --uninstall is a maintenance command; never combine with transfer.
	if f.uninstall {
		return runUninstall(f)
	}

	if !validMode(f.mode) {
		return fmt.Errorf("%w: invalid --mode %q (expected: local, stun, or turn)", fserrors.ErrUsage, f.mode)
	}
	if f.forceSend && f.forceReceive {
		return fmt.Errorf("%w: --send and --receive are mutually exclusive", fserrors.ErrUsage)
	}

	// Env-var fallback for the password (FSEND_PASS). Passing a secret via
	// flag leaks it through /proc/<pid>/cmdline and `ps -ef`; the env var
	// lets users keep it out of argv. We only consult it when the flag
	// wasn't explicitly given, so scripts that set both keep the flag's
	// value (matching every other CLI's override convention).
	applyEnvFallbacks(f, cmd)

	// Bare --pass (no value) is resolved inside runSend / runReceive so
	// each role can show the right prompt: the sender gets a random
	// 16-char suggestion, the receiver gets a hidden no-echo prompt.

	// Implicit stdin: no args + non-TTY stdin (pipe or redirected file)
	// means "send what's coming in". Bare `fsend` in an interactive
	// shell hits the help path below because stdin is a TTY there.
	if len(f.posArgs) == 0 && !f.forceReceive && !cmd.Flags().Changed("text") &&
		!term.IsTerminal(int(os.Stdin.Fd())) {
		f.posArgs = []string{"-"}
	}

	// Force mode short-circuits auto-detect.
	if f.forceSend {
		return runSend(f, f.posArgs)
	}
	if f.forceReceive {
		if len(f.posArgs) != 1 {
			return fmt.Errorf("%w: --receive requires exactly one positional argument (the code)", fserrors.ErrUsage)
		}
		return runReceive(f, f.posArgs[0])
	}

	// --text is unambiguously send mode. An explicitly empty value
	// (--text "") is a usage error, not a fall-through-to-help: the
	// user clearly meant to send something.
	if cmd.Flags().Changed("text") {
		if f.textArg == "" {
			return fmt.Errorf("%w: --text requires a non-empty string", fserrors.ErrUsage)
		}
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
	// Codes copied through iMessage / WhatsApp / Slack often have the
	// first letter auto-capitalized. If the lowercased form is a valid
	// code AND there's no file with the original name, accept it as a
	// receive — anything else (file exists, or lowercased still doesn't
	// match the regex) falls through to send.
	if lowered := strings.ToLower(arg); lowered != arg && code.IsCode(lowered) {
		if _, err := os.Stat(arg); os.IsNotExist(err) {
			return runReceive(f, lowered)
		}
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
	fmt.Fprintf(os.Stderr, "  [s]end this file, or [r]eceive with this code? (r): ")

	resp := readLine(os.Stdin)
	switch resp {
	case "s", "send":
		return runSend(f, []string{arg})
	case "r", "receive", "":
		// Default on bare <enter>: receive. Codes are the surprising
		// case (a filename happens to match the regex); receiving is
		// almost always what the user meant.
		return runReceive(f, arg)
	default:
		return fmt.Errorf("%w: choose 's' or 'r'", fserrors.ErrUsage)
	}
}
