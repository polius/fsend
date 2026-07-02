package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/uxlog"
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
	preview  bool     // list what would be sent (CSV), then exit — no transfer

	// Receive-side
	yes       bool
	outDir    string
	overwrite bool
	checksum  bool
	manifest  string // write a CSV record of the received files to this path

	// Server selection — connectArgsRaw is the raw slice cobra hands
	// back; "no flag" vs "empty flag" is distinguished via Flags().Changed.
	connectArgsRaw []string

	// Debug-only sender path override: "local" | "direct" | "relay".
	// Forces a specific data path instead of the default LAN+pairing-server
	// race with ICE-then-relay fallback. Hidden from --help, undocumented.
	mode string

	// Misc
	quiet     bool
	debug     bool
	update    bool
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
	ht = boldHelpHeaders(ht)
	c.SetHelpTemplate(ht)
	c.SetUsageTemplate(ht)

	// Transfer behavior
	c.Flags().StringVar(&f.textArg, "text", "", "send a literal string instead of a file")
	c.Flags().StringVar(&f.passArg, "password", "",
		"password gate. Bare --password prompts (sender: suggests a random default); inline: --password=SECRET. Env: FSEND_PASSWORD")
	// Bare --password (no value) collapses to this sentinel; the dispatch
	// layer treats it as "ask interactively, with input hidden." We
	// blank DefValue so cobra's --help doesn't print the sentinel.
	passFlag := c.Flags().Lookup("password")
	passFlag.NoOptDefVal = passPromptSentinel
	passFlag.DefValue = ""
	c.Flags().BoolVar(&f.yes, "yes", false, "auto-accept incoming transfers")
	c.Flags().StringVar(&f.outDir, "out", "", "receive into this directory")
	c.Flags().BoolVar(&f.overwrite, "overwrite", false, "overwrite existing files that differ on receive")
	c.Flags().BoolVar(&f.checksum, "checksum", false, "decide identical files by content hash, not size+mtime (like rsync -c)")
	c.Flags().BoolVar(&f.preview, "preview", false, "list what would be sent (CSV: path,size) and exit; no transfer")
	c.Flags().StringVar(&f.manifest, "manifest", "", "write a CSV record (path,size,status) of the received files to this path")
	c.Flags().BoolVar(&f.quiet, "quiet", false, "suppress non-error output")
	c.Flags().StringVar(&f.hostname, "name", "", "override the hostname shown to the peer")
	c.Flags().StringSliceVar(&f.excludes, "exclude", nil,
		"glob patterns to skip when bundling a directory (repeatable or comma-separated)")

	// Mode override
	c.Flags().BoolVar(&f.forceSend, "send", false, "force send mode (skip auto-detect)")
	c.Flags().BoolVar(&f.forceReceive, "receive", false, "force receive mode (skip auto-detect)")

	// Server selection. Bare --connect (no value) means "show current
	// server" — same NoOptDefVal trick as --password. The dispatcher
	// recognises the sentinel and treats it as "no args".
	c.Flags().StringSliceVar(&f.connectArgsRaw, "connect", nil, "set the server: <host[:port]>[,<password>] | 'default'")
	connectFlag := c.Flags().Lookup("connect")
	connectFlag.NoOptDefVal = connectShowSentinel
	connectFlag.DefValue = ""

	// Misc
	c.Flags().BoolVar(&f.debug, "debug", false, "verbose logging to stderr")
	c.Flags().BoolVar(&f.update, "update", false, "update fsend to the latest release")
	c.Flags().BoolVar(&f.uninstall, "uninstall", false, "remove the fsend binary and config dir")

	c.Flags().StringVar(&f.mode, "mode", "", "")
	_ = c.Flags().MarkHidden("mode")

	c.AddCommand(serverCmd())

	// Replace cobra's auto-generated completion command: it inherits the
	// root help/usage template (so `fsend completion --help` printed the
	// root page) and exits 0 on a missing or unknown shell — fatal when
	// the output is eval'd in a dotfile.
	c.CompletionOptions.DisableDefaultCmd = true
	c.AddCommand(completionCmd())

	return c
}

// completionCmd prints a completion script for one of the supported
// shells, with a usage error (E024, nonzero exit) for anything else.
func completionCmd() *cobra.Command {
	const shells = "bash, zsh, fish, powershell"
	c := &cobra.Command{
		Use:           "completion <shell>",
		Short:         "Print a shell completion script",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return fmt.Errorf("%w: completion expects one shell argument (%s)", fserrors.ErrUsage, shells)
			}
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			}
			return fmt.Errorf("%w: unknown shell %q (expected one of: %s)", fserrors.ErrUsage, args[0], shells)
		},
	}
	ht := boldHelpHeaders(completionHelpTemplate)
	c.SetHelpTemplate(ht)
	c.SetUsageTemplate(ht)
	return c
}

const completionHelpTemplate = `fsend completion — print a shell completion script

USAGE
  fsend completion <bash|zsh|fish|powershell>

EXAMPLES
  zsh:   eval "$(fsend completion zsh)"
  bash:  eval "$(fsend completion bash)"
  fish:  fsend completion fish | source

  Add the line to your shell's rc file to load it on every session.
`

// validMode reports whether s is an accepted --mode value.
// "" means "no override" (default auto-selection).
func validMode(s string) bool {
	switch s {
	case "", modeLocal, modeDirect, modeRelay:
		return true
	}
	return false
}

// --mode values name the *path* the sender is forced down, not any
// underlying network protocol. The historical names "stun" and "turn"
// were colloquial labels for "ICE direct" and "relay" respectively;
// since the server no longer plays the role of a STUN reflector or a
// TURN-spec relay (it runs a custom token-keyed UDP forwarder), the
// flag values now describe what they actually do.
const (
	modeLocal  = "local"
	modeDirect = "direct"
	modeRelay  = "relay"
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
    fsend report.pdf --password="shared-secret"
  Use a different server:
    fsend --connect relay.mycompany.com:443

SENDING
  --password             Require a password to receive. Bare --password prompts;
                         inline --password=SECRET; env FSEND_PASSWORD.
  --exclude <glob,…>     Skip entries matching these globs in a directory
  --text "<string>"      Send a literal string instead of a file
                         (the receiver prints it — nothing is saved;
                         keep it with: fsend <code> > note.txt)
  --preview              List what would be sent (CSV: path,size) and exit —
                         no code, no transfer (pipe-friendly: --preview > out.csv)
  --name <string>        Override the hostname shown to the peer

RECEIVING
  --yes                  Auto-accept incoming transfers (no prompt)
  --out <dir>            Receive into this directory (default: current)
  --out -                Receive to stdout (single file, text, or piped
                         stream — pipe-friendly: fsend <code> --out - | …)
  --password             Supply the sender's password. Bare --password prompts;
                         inline --password=SECRET; env FSEND_PASSWORD.
  --overwrite            Replace existing files that differ (identical files
                         are always skipped)
  --checksum             Decide identical files by content hash, not
                         size+mtime (like rsync -c)
  --manifest <file>      Write a CSV record (path,size,status) of the
                         received files to <file>

GENERAL
  --quiet                Suppress all non-error output
  --debug                Verbose logging to stderr (also: FSEND_DEBUG=1)
  --help, -h             Show this help
  --version, -v          Show version

ADVANCED
  --connect              Show current server
  --connect <host:port>  Set the server (persisted)
  --connect <host:port>,<password>
                         Set the server + shared password
  --connect default      Revert to the compiled-in default server
  --send / --receive     Force mode (skip code/path auto-detect)
  --update               Update fsend to the latest release
  --uninstall            Remove the fsend binary and its config dir

LEARN MORE
  https://github.com/polius/fsend
`

// boldHelpHeaders wraps the ALL-CAPS section headers (USAGE, EXAMPLES,
// …) of a help template in ANSI bold so the page scans by section.
// Gated on stdout — where cobra writes help — so pipes, NO_COLOR, and
// non-TTY contexts get the template byte-for-byte untouched.
func boldHelpHeaders(tpl string) string {
	if !uxlog.ColorFor(os.Stdout) {
		return tpl
	}
	lines := strings.Split(tpl, "\n")
	for i, line := range lines {
		if isHelpHeader(line) {
			lines[i] = uxlog.Bold(line)
		}
	}
	return strings.Join(lines, "\n")
}

// isHelpHeader reports whether line is a column-0 ALL-CAPS section
// header ("COMMON FLAGS", "ADVANCED FLAGS", …). Body lines are indented,
// and the version header line starts lowercase, so neither matches.
func isHelpHeader(line string) bool {
	if line == "" || (line[0] < 'A' || line[0] > 'Z') {
		return false
	}
	for _, r := range line {
		switch {
		case r >= 'A' && r <= 'Z', r == ' ', r == '-':
		default:
			return false
		}
	}
	return true
}

// passPromptSentinel is the value cobra hands us when the user passes
// bare --password with no argument. The dispatch layer translates this into
// an interactive no-echo prompt before any send/receive work begins.
//
// Note: a user who explicitly passes --password=":prompt:" gets the same
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
		for _, conflict := range []string{"send", "receive", "text", "password", "yes", "out", "overwrite", "name", "exclude", "mode", "update", "uninstall"} {
			if cmd.Flags().Changed(conflict) {
				return fmt.Errorf("%w: --connect cannot be combined with --%s", fserrors.ErrUsage, conflict)
			}
		}
		// Positionals after --connect are a usage error — without this,
		// `fsend --connect host:port file.pdf` would silently fall through
		// to the send path with --connect already consumed.
		if len(f.posArgs) > 0 {
			return fmt.Errorf("%w: --connect cannot be combined with positional arguments (got %q); "+
				"to set a server password use --connect host:port,password",
				fserrors.ErrUsage, f.posArgs[0])
		}
		// `fsend --connect abc-defg-jkm`: the code rides with the flag and
		// would be persisted as the server host. Strict IsCode only —
		// LooksLikeCode would reject legitimate hyphenated hostnames.
		if len(f.connectArgsRaw) == 1 && code.IsCode(f.connectArgsRaw[0]) {
			return fmt.Errorf("%w: --connect consumed %q as the server address; to receive: fsend %s",
				fserrors.ErrUsage, f.connectArgsRaw[0], f.connectArgsRaw[0])
		}
		return runConnect(f)
	}

	// --update / --uninstall are maintenance commands; never combine
	// with transfer or with each other.
	if f.update && f.uninstall {
		return fmt.Errorf("%w: --update and --uninstall are mutually exclusive", fserrors.ErrUsage)
	}
	if f.update {
		return runUpdate()
	}
	if f.uninstall {
		return runUninstall(f)
	}

	if !validMode(f.mode) {
		return fmt.Errorf("%w: invalid --mode %q (expected: local, direct, or relay)", fserrors.ErrUsage, f.mode)
	}
	if f.forceSend && f.forceReceive {
		return fmt.Errorf("%w: --send and --receive are mutually exclusive", fserrors.ErrUsage)
	}
	// An empty positional ("" from a botched shell expansion) would fall
	// through to the send path as a nameless E025.
	for _, a := range f.posArgs {
		if a == "" {
			return fmt.Errorf("%w: empty argument (check your shell quoting)", fserrors.ErrUsage)
		}
	}

	// `--password=` (explicitly empty): the user asked for a password gate but
	// supplied none — proceeding would run the transfer unprotected.
	if cmd.Flags().Changed("password") && f.passArg == "" {
		return fmt.Errorf("%w: --password requires a non-empty password (bare --password prompts interactively)",
			fserrors.ErrUsage)
	}

	// An inline password now rides the flag as `--password=secret`, so a bare
	// --password followed by a non-file, non-code word is very likely a misplaced
	// inline password (`fsend --password secret file`). Point at the = form rather
	// than failing later with a bare "no such file".
	if cmd.Flags().Changed("password") && f.passArg == passPromptSentinel &&
		!cmd.Flags().Changed("text") && !f.forceReceive {
		for _, a := range f.posArgs {
			if a == "-" || code.IsCode(a) || code.LooksLikeCode(a) {
				continue
			}
			if _, err := os.Stat(a); os.IsNotExist(err) {
				return fmt.Errorf("%w: %q is not a file; if it's the password, use --password=%s (bare --password prompts)",
					fserrors.ErrUsage, a, a)
			}
		}
	}

	// Env-var fallback for the password (FSEND_PASSWORD). Passing a secret via
	// flag leaks it through /proc/<pid>/cmdline and `ps -ef`; the env var
	// lets users keep it out of argv. We only consult it when the flag
	// wasn't explicitly given, so scripts that set both keep the flag's
	// value (matching every other CLI's override convention).
	applyEnvFallbacks(f, cmd)

	// Bare --password (no value) is resolved inside runSend / runReceive so
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
		// --text is a send-side flag; combined with --receive it would
		// otherwise be silently ignored. Reject rather than drop it.
		if cmd.Flags().Changed("text") {
			return fmt.Errorf("%w: --text cannot be combined with --receive", fserrors.ErrUsage)
		}
		if len(f.posArgs) != 1 {
			return fmt.Errorf("%w: --receive requires exactly one positional argument (the code)", fserrors.ErrUsage)
		}
		return startReceive(f, f.posArgs[0])
	}

	// --text is unambiguously send mode. An explicitly empty value
	// (--text "") is a usage error, not a fall-through-to-help: the
	// user clearly meant to send something.
	if cmd.Flags().Changed("text") {
		if f.textArg == "" {
			return fmt.Errorf("%w: --text requires a non-empty string", fserrors.ErrUsage)
		}
		// Don't silently drop files the user also listed; runSend's own
		// guard never sees them otherwise (we'd pass nil).
		if len(f.posArgs) > 0 {
			return fmt.Errorf("%w: --text cannot be combined with file arguments", fserrors.ErrUsage)
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
	// A code wrapped in whitespace (a redirected newline, a pasted space, a
	// CRLF) is never a filename, so receive the trimmed form. --send forces
	// send for a file genuinely named like a code.
	if c, ok := wrappedCode(arg); ok {
		return startReceive(f, c)
	}
	if code.IsCode(arg) {
		// Code regex match. If a file with that name exists in CWD, prompt.
		if _, err := os.Stat(arg); err == nil {
			return promptCodeOrPath(f, arg)
		}
		return startReceive(f, arg)
	}
	// Codes copied through iMessage / WhatsApp / Slack often have the
	// first letter auto-capitalized. If the lowercased form is a valid
	// code AND there's no file with the original name, accept it as a
	// receive — anything else (file exists, or lowercased still doesn't
	// match the regex) falls through to send.
	if lowered := strings.ToLower(arg); lowered != arg && code.IsCode(lowered) {
		if _, err := os.Stat(arg); os.IsNotExist(err) {
			return startReceive(f, lowered)
		}
	}
	return runSend(f, []string{arg})
}

// wrappedCode reports a share code surrounded by whitespace, returning it
// trimmed (and lowercased — a chat-app copy that grabs a trailing space often
// also capitalizes the first letter). A clean code returns false, leaving it to
// the code-vs-path logic below; internal whitespace is untouched, so a
// malformed code stays one.
func wrappedCode(arg string) (string, bool) {
	t := strings.TrimSpace(arg)
	if t == arg {
		return "", false
	}
	if code.IsCode(t) {
		return t, true
	}
	if low := strings.ToLower(t); code.IsCode(low) {
		return low, true
	}
	return "", false
}

// applyEnvFallbacks fills in flags from environment variables when the
// user did not pass the corresponding flag. Used for secrets we'd
// rather not see on argv.
//
// Bare --password (collapsed to passPromptSentinel) counts as "the user said
// they want a password, but didn't supply one" — so we still consult
// FSEND_PASSWORD. Doing it the other way around (env wins only when the
// flag is absent entirely) silently ignored FSEND_PASSWORD for users who
// typed `--password` to opt in.
func applyEnvFallbacks(f *flags, cmd *cobra.Command) {
	bare := f.passArg == passPromptSentinel
	if !cmd.Flags().Changed("password") || bare {
		if v := os.Getenv("FSEND_PASSWORD"); v != "" {
			f.passArg = v
		}
	}
}

// startReceive rejects send-only flags before handing off to runReceive,
// so `fsend <code> --exclude '*.log'` fails fast instead of silently
// ignoring the flag.
func startReceive(f *flags, c string) error {
	if len(f.excludes) > 0 {
		return errExcludeMisuse()
	}
	if f.preview {
		return fmt.Errorf("%w: --preview is a send-side flag and has no effect when receiving", fserrors.ErrUsage)
	}
	return runReceive(f, c)
}

// promptCodeOrPath handles the rare collision case where a CLI arg both
// matches the code regex AND names a real file in CWD.
func promptCodeOrPath(f *flags, arg string) error {
	send, err := askCodeOrPath(stdinReader(), arg)
	if err != nil {
		return err
	}
	if send {
		return runSend(f, []string{arg})
	}
	return startReceive(f, arg)
}

// askCodeOrPath runs the send-or-receive disambiguation prompt. Default
// on bare <enter>: receive — codes are the surprising case (a filename
// happens to match the regex), so receiving is almost always what the
// user meant. EOF must not take that default (see the readLine contract);
// unrecognized input re-prompts instead of hard-erroring on a typo.
func askCodeOrPath(br *bufio.Reader, arg string) (send bool, err error) {
	fmt.Fprintf(os.Stderr, "\n  %q matches a code AND is a local file.\n", arg)
	for {
		fmt.Fprintf(os.Stderr, "  [s]end this file, or [r]eceive with this code? (r): ")
		resp, eof := readLineFrom(br)
		if eof {
			fmt.Fprintln(os.Stderr)
			return false, fmt.Errorf("%w: no input to answer the code-vs-file prompt; pass --receive (or --send) to choose without a prompt",
				fserrors.ErrUsage)
		}
		switch resp {
		case "s", "send":
			return true, nil
		case "r", "receive", "":
			return false, nil
		}
		fmt.Fprintln(os.Stderr, "  Please answer s or r.")
	}
}
