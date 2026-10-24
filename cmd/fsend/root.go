package main

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
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
	as       string   // file name for a piped stream on the receiver's disk

	// Per-run server override (--server; FSEND_SERVER), never persisted.
	server string

	// Receive-side
	yes       bool
	outDir    string
	overwrite bool
	rename    bool // keep both: save differing incoming files as "name (1).ext"
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
	json      bool // NDJSON events on stdout; human output stays on stderr
	debug     bool
	update    bool
	check     bool // with --update: report only, install nothing
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
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		// Typo'd positionals reach dispatch as file paths (Args accepts
		// anything), so the "did you mean" for those lives in renderError;
		// this covers cobra's own unknown-subcommand path.
		SuggestionsMinimumDistance: 2,
		// Every path and code is a positional: complete files and dirs.
		ValidArgsFunction: func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
			return nil, cobra.ShellCompDirectiveDefault
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			f.posArgs = args
			return dispatch(cmd, f)
		},
	}

	// Route cobra's own flag-parse errors ("flag needs an argument",
	// "unknown flag", ...) through ErrUsage so they render with the
	// catalog [E024] line instead of dropping into the E099 "please
	// file an issue" catchall — with a "did you mean" for a typo'd flag.
	c.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return fmt.Errorf("%w: %v%s", fserrors.ErrUsage, err, flagSuggestion(cmd, err))
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
	ht := renderHelp(helpTemplate)
	c.SetHelpTemplate(ht)
	c.SetUsageTemplate(ht)
	c.SetHelpCommand(helpCmd())

	// Old spellings and other tools' habits, resolved to the real flag:
	// --name was the device-name flag before --device-name; --output is
	// what curl/wget users type for --out.
	c.Flags().SetNormalizeFunc(func(_ *pflag.FlagSet, name string) pflag.NormalizedName {
		switch name {
		case "name":
			name = "device-name"
		case "output":
			name = "out"
		}
		return pflag.NormalizedName(name)
	})

	// Transfer behavior
	c.Flags().StringVar(&f.textArg, "text", "", "send a literal string instead of a file")
	c.Flags().StringVar(&f.as, "as", "", "file name the receiver saves a piped stream as")
	c.Flags().StringVar(&f.passArg, "password", "",
		"password gate. Bare --password prompts (sender: suggests a random default); inline: --password=SECRET. Env: FSEND_PASSWORD (or FSEND_PASSWORD_FILE)")
	// Bare --password (no value) collapses to this sentinel; the dispatch
	// layer treats it as "ask interactively, with input hidden." We
	// blank DefValue so cobra's --help doesn't print the sentinel.
	passFlag := c.Flags().Lookup("password")
	passFlag.NoOptDefVal = passPromptSentinel
	passFlag.DefValue = ""
	c.Flags().BoolVarP(&f.yes, "yes", "y", false, "auto-accept incoming transfers (differing files are kept unless --overwrite or --rename)")
	c.Flags().StringVarP(&f.outDir, "out", "o", "", "receive into this directory, created if missing")
	_ = c.MarkFlagDirname("out")
	c.Flags().BoolVar(&f.overwrite, "overwrite", false, "overwrite existing files that differ on receive")
	c.Flags().BoolVar(&f.rename, "rename", false, "keep both: save incoming files that differ as \"name (1).ext\"")
	c.Flags().BoolVar(&f.checksum, "checksum", false, "decide identical files by content hash, not size+mtime (like rsync -c)")
	c.Flags().BoolVar(&f.preview, "preview", false, "list what would be sent (CSV: path,size) and exit; no transfer")
	c.Flags().StringVar(&f.manifest, "manifest", "", "write a CSV record (path,size,status,sha256) of the received files to this path")
	c.Flags().BoolVarP(&f.quiet, "quiet", "q", false, "suppress all non-error output (send still prints the bare code to stdout)")
	c.Flags().BoolVar(&f.json, "json", false, "emit NDJSON events (code, final summary) on stdout")
	c.Flags().StringVar(&f.hostname, "device-name", "", "override the device name shown to the peer (default: hostname)")
	c.Flags().StringVar(&f.server, "server", "", "use this server for this run only (not saved)")
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
	// Acted on before cobra parses (see main): the help template is built
	// — and flag-parse errors render — before RunE ever runs.
	c.Flags().BoolVar(&f.update, "update", false, "update fsend to the latest release")
	c.Flags().BoolVar(&f.check, "check", false, "with --update: report whether a newer release exists, then exit")
	c.Flags().BoolVar(&f.uninstall, "uninstall", false, "remove the fsend binary and config dir")

	c.Flags().StringVar(&f.mode, "mode", "", "")
	_ = c.Flags().MarkHidden("mode")

	c.AddCommand(serverCmd())
	c.AddCommand(doctorCmd())

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
  zsh:         eval "$(fsend completion zsh)"
  bash:        eval "$(fsend completion bash)"
  fish:        fsend completion fish | source
  powershell:  fsend completion powershell | Out-String | Invoke-Expression

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

// helpTemplate is the everyday help page: `fsend --help`, and bare `fsend`
// off a terminal. It lists the flags most runs need; the rest live in
// advancedHelpTemplate (`fsend help advanced`). Hand-written rather than
// cobra's alphabetised flag wall so wording and grouping stay ours.
const helpTemplate = `fsend — peer-to-peer file transfer

USAGE
  fsend <file|dir>...          Send (prints a code for the other side)
  fsend <code>                 Receive (a code like abc-defg-jkm)
  cat file.txt | fsend         Send from a pipe (or: fsend < file)
  fsend --text "hello world"   Send a literal string (the receiver prints it)

SENDING
  --password             Require a password; the receiver passes it too.
                         Bare --password prompts; or --password=SECRET
  --exclude <glob,…>     Skip entries matching these globs in a folder
  --as <name>            File name the receiver saves a piped stream as
  --preview              List what would be sent (CSV: path,size) and exit

RECEIVING
  -y, --yes              Accept without asking (differing files are kept
                         unless --overwrite or --rename)
  -o, --out <dir>        Save into <dir>, created if missing; - for stdout
                         (a folder arrives as a tar: fsend <code> -o - | tar x)
  --overwrite            Replace existing files that differ (folders are
                         never deleted)
  --rename               Keep both: save differing files as "name (1).ext"

GENERAL
  -q, --quiet            Errors only (send still prints the code to stdout)
  --json                 NDJSON events on stdout (schema: docs/usage.md)
  --server <host>        Use this server for this run only, not saved
  -h, --help             Show this help
  -v, --version          Show version

  More flags, environment variables, exit status:  fsend help advanced

LEARN MORE
  https://github.com/polius/fsend
`

// advancedHelpTemplate is `fsend help advanced`: everything the everyday
// page leaves out.
const advancedHelpTemplate = `fsend — advanced options

MORE FLAGS
  --checksum             Decide identical files by content hash, not
                         size+mtime (like rsync -c)
  --manifest <file>      Write a CSV record (path,size,status,sha256) of the
                         received files to <file>
  --device-name <name>   Device name shown to the peer (default: hostname)
  --send / --receive     Force a mode (skip code/path auto-detect);
                         bare --receive asks for the code
  --debug                Verbose logging to stderr (also: FSEND_DEBUG=1)

SERVER
  --connect              Show the saved server
  --connect <host[:port]>[,<password>]
                         Save a server (and its password) for every run.
                         HTTPS unless you write http://host
  --connect default      Back to the built-in server
  --server <host>        Use a server for this run only (or FSEND_SERVER)

MAINTENANCE
  --update               Update fsend to the latest release
  --update --check       Report whether a newer release exists
  --uninstall            Remove the fsend binary and its config dir
  fsend doctor           Check your setup
  fsend server           Run your own pairing/relay server (--help for more)
  fsend completion <shell>
                         Print a tab-completion script (bash, zsh, fish,
                         powershell)

ENVIRONMENT
  FSEND_CODE             The code to receive, kept out of the process list
  FSEND_PASSWORD         Transfer password (or FSEND_PASSWORD_FILE)
  FSEND_SERVER           Server for this run, like --server
  FSEND_SERVER_PASSWORD  Server password (or FSEND_SERVER_PASSWORD_FILE)
  FSEND_THEME            light or dark: match colours to your background
  FSEND_PLAIN=1          No colour, animation or Unicode symbols (screen
                         readers, logs); automatic when the locale isn't UTF-8
  FSEND_NO_NOTIFY=1      No bell or desktop notification after long runs
  NO_COLOR=1             No colour

EXIT STATUS
  0 on success, or when you decline at a prompt; 130 on Ctrl-C; otherwise
  the error's number (E013 exits 13). The table is in docs/usage.md.
`

// renderHelp stamps the version into a help template's header and adds
// the colour decoration (bold headers, accented flags) when stdout takes it.
// The header carries the version so users see what they're running on
// every --help — saves a round-trip when writing a bug report.
func renderHelp(tpl string) string {
	tpl = strings.Replace(tpl, "fsend —", "fsend "+version.Version+" —", 1)
	return decorateHelpFlags(boldHelpHeaders(tpl))
}

// helpCmd replaces cobra's help command, which printed the root page for
// any unknown topic: `fsend help xyz` now fails with E024 and the topics.
func helpCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "help [advanced|server|doctor|completion]",
		Short:         "Help about fsend",
		SilenceUsage:  true,
		SilenceErrors: true,
		ValidArgs:     []string{"advanced", "server", "doctor", "completion"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch {
			case len(args) == 0:
				return root.Help()
			case len(args) == 1 && args[0] == "advanced":
				_, err := fmt.Fprint(root.OutOrStdout(), renderHelp(advancedHelpTemplate))
				return err
			}
			if sub, rest, err := root.Find(args); err == nil && sub != root && len(rest) == 0 {
				return sub.Help()
			}
			return fmt.Errorf("%w: unknown help topic %q (topics: advanced, server, doctor, completion)",
				fserrors.ErrUsage, strings.Join(args, " "))
		},
	}
}

// flagSuggestion turns pflag's "unknown flag: --overwirte" into a hint
// naming the closest real flag. "" when nothing is close.
func flagSuggestion(cmd *cobra.Command, err error) string {
	_, name, ok := strings.Cut(err.Error(), "unknown flag: --")
	if !ok {
		return ""
	}
	name, _, _ = strings.Cut(name, "=")
	var names []string
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			names = append(names, f.Name)
		}
	})
	if best := closest(name, names); best != "" {
		return " (did you mean --" + best + "?)"
	}
	return ""
}

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

// stdoutIsTTY reports whether stdout is an interactive terminal. A var so
// tests can flip it without faking a file descriptor.
var stdoutIsTTY = func() bool { return term.IsTerminal(int(os.Stdout.Fd())) }

// startHere is what bare `fsend` prints on an interactive terminal: the two
// commands a newcomer actually needs, then a pointer to the reference. The
// full manual stays behind --help, and a piped or non-TTY invocation keeps
// printing it too, so scripts and `fsend | less` still see every flag.
func startHere() {
	colour := uxlog.ColorFor(os.Stdout)
	// Bold, Brand and Accent are unconditional in uxlog (they decorate
	// --help, which cobra writes to stdout); the gate lives here.
	paint := func(render func(string) string, s string) string {
		if !colour {
			return s
		}
		return render(s)
	}
	_, _ = fmt.Fprintf(os.Stdout, `  %s  %s — send files straight to another computer

  %s
    fsend %s

  %s
    fsend %s       ← the code fsend gives you

  More: fsend %s   everything
        fsend %s   check your setup
`,
		paint(uxlog.Brand, "⇄"),
		paint(uxlog.Bold, "fsend"),
		paint(uxlog.Bold, "Send a file"),
		paint(uxlog.Path, "report.pdf"),
		paint(uxlog.Bold, "Then, on the other machine"),
		paint(uxlog.Code, "xqp-wjyc-zxk"),
		paint(uxlog.Accent, "--help"),
		paint(uxlog.Accent, "doctor"),
	)
}

// flagTokenRe matches a single-dash or double-dash token ("--out", "-h")
// inside a help entry's flag column. Descriptions live right of the
// column gap and never contain a leading-dash token at a word boundary
// that would matter — only the left side is scanned.
var flagTokenRe = regexp.MustCompile(`-{1,2}[A-Za-z][A-Za-z0-9-]*`)

// decorateHelpFlags colours the hand-written flag reference: flag tokens
// cyan so each entry scans as [flag] [description] and the eye can jump
// between flags without reading prose. Descriptions stay in the
// terminal's default foreground — readable by definition; only headers
// (bold) and flags (cyan) are emphasized.
//
// Line shapes handled (indentation-keyed):
//   - "  --flag <arg>        description" — split at the first 2+ space
//     run after the indent; dash tokens on the left go cyan.
//   - continuation lines are left untouched.
//
// Gated on stdout — cobra writes help there — so pipes, NO_COLOR, and
// non-TTY contexts get the template byte-for-byte untouched. The
// completion template keeps boldHelpHeaders only: its EXAMPLES lines
// ("zsh:  eval …") share the entry shape but are not a flag reference.
func decorateHelpFlags(tpl string) string {
	if !uxlog.ColorFor(os.Stdout) {
		return tpl
	}
	lines := strings.Split(tpl, "\n")
	for i, line := range lines {
		// LEARN MORE footer: the URL renders in the info accent (the
		// markdown-link role).
		if strings.HasPrefix(line, "  https://") {
			lines[i] = uxlog.Link(line)
			continue
		}
		if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
			continue
		}
		gap := helpColumnGap(line)
		if gap < 0 {
			continue // no description on this line ("  --connect <host[:port]>")
		}
		lines[i] = colorizeFlagTokens(line[:gap]) + line[gap:]
	}
	return strings.Join(lines, "\n")
}

// helpColumnGap returns the index of the first 2+ space run at or after
// the two-space indent — the boundary between the flag column and its
// description. -1 when the line has no description.
func helpColumnGap(line string) int {
	for i := 2; i+1 < len(line); i++ {
		if line[i] == ' ' && line[i+1] == ' ' {
			return i
		}
	}
	return -1
}

// colorizeFlagTokens wraps every dash token in s with the cyan accent.
func colorizeFlagTokens(s string) string {
	return flagTokenRe.ReplaceAllStringFunc(s, func(m string) string {
		return uxlog.Accent(m)
	})
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

// maintenanceGuard rejects transfer flags and positionals alongside a
// maintenance flag (--update / --uninstall): anything else on the command
// line would silently not run. allowYes exempts --yes, which --uninstall
// documents as skipping its confirmation prompt.
func maintenanceGuard(cmd *cobra.Command, f *flags, name string, allowYes bool) error {
	for _, conflict := range []string{"send", "receive", "text", "password", "yes", "out", "overwrite", "rename", "device-name", "exclude", "mode", "checksum", "manifest", "preview", "json", "server", "as"} {
		if conflict == "yes" && allowYes {
			continue
		}
		if cmd.Flags().Changed(conflict) {
			return fmt.Errorf("%w: %s cannot be combined with --%s", fserrors.ErrUsage, name, conflict)
		}
	}
	if len(f.posArgs) > 0 {
		return fmt.Errorf("%w: %s cannot be combined with positional arguments (got %q)", fserrors.ErrUsage, name, f.posArgs[0])
	}
	return nil
}

// dispatch implements the CLI dispatch rules documented on the main.go
// package comment.
func dispatch(cmd *cobra.Command, f *flags) error {
	if f.json {
		jsonEnable()
	}
	// Handle --connect (server configuration) before anything else.
	// Reject pairings with transfer-mode flags so the user can't be
	// surprised by "I asked for both --connect and --send and only the
	// first ran."
	if cmd.Flags().Changed("connect") {
		for _, conflict := range []string{"send", "receive", "text", "password", "yes", "out", "overwrite", "rename", "device-name", "exclude", "mode", "update", "check", "uninstall", "checksum", "manifest", "preview", "json", "server", "as"} {
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
	// with transfer flags, positionals, or each other. Same rationale as
	// --connect above: `fsend report.pdf --update` would otherwise run
	// the updater and silently drop the file the user asked to send.
	if f.update && f.uninstall {
		return fmt.Errorf("%w: --update and --uninstall are mutually exclusive", fserrors.ErrUsage)
	}
	if f.check && !f.update {
		return fmt.Errorf("%w: --check reports on --update; run: fsend --update --check", fserrors.ErrUsage)
	}
	if f.update {
		if err := maintenanceGuard(cmd, f, "--update", false); err != nil {
			return err
		}
		return runUpdate(f.check)
	}
	if f.uninstall {
		// --yes stays allowed: it skips the uninstall confirmation.
		if err := maintenanceGuard(cmd, f, "--uninstall", true); err != nil {
			return err
		}
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
	// than failing later with a bare "no such file". With explicit --send the
	// positional is unambiguously a path, so skip the guess and let the real
	// error (the missing source, or the bare-password prompt) stand.
	if cmd.Flags().Changed("password") && f.passArg == passPromptSentinel &&
		!cmd.Flags().Changed("text") && !f.forceReceive && !f.forceSend {
		for _, a := range f.posArgs {
			if _, ok := code.Normalize(a); ok || a == "-" || code.LooksLikeCode(a) {
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
	if err := applyEnvFallbacks(f, cmd); err != nil {
		return err
	}

	// Bare --password (no value) is resolved inside runSend / runReceive so
	// each role can show the right prompt: the sender gets a random
	// four-word suggestion, the receiver gets a hidden no-echo prompt.

	// --server / FSEND_SERVER: validated before any work so a typo fails
	// before a code is issued.
	if err := setServerOverride(f.server); err != nil {
		return err
	}

	// FSEND_CODE receives without the code ever sitting in argv, where any
	// local user can read it from `ps`. It beats implicit stdin: a CI job
	// that sets it means to receive, whatever its stdin happens to be.
	if len(f.posArgs) == 0 && !f.forceSend && !f.forceReceive && !cmd.Flags().Changed("text") {
		if c := os.Getenv("FSEND_CODE"); c != "" {
			return startReceive(f, c)
		}
	}

	// Implicit stdin: no args and stdin carries data (a pipe, or a
	// non-empty redirected file) means "send what's coming in". A terminal,
	// /dev/null or a closed stdin (cron, CI, `ssh host fsend`) is not a
	// send — it falls through to help instead of publishing an empty stream.
	if len(f.posArgs) == 0 && !f.forceReceive && !cmd.Flags().Changed("text") && stdinHasData() {
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
		switch len(f.posArgs) {
		case 0:
			c, err := askForCode()
			if err != nil {
				return err
			}
			return startReceive(f, c)
		case 1:
			return startReceive(f, f.posArgs[0])
		}
		return fmt.Errorf("%w: --receive requires exactly one positional argument (the code)", fserrors.ErrUsage)
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
			return fmt.Errorf("%w: --text cannot be combined with positional arguments", fserrors.ErrUsage)
		}
		return runSend(f, nil)
	}

	// No positional args and no flag-driven send: on an interactive
	// terminal show the short "start here" screen; anywhere else (a pipe,
	// CI, `fsend | less`) keep printing the full reference.
	if len(f.posArgs) == 0 {
		if stdoutIsTTY() {
			startHere()
			return nil
		}
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
		return startReceive(f, arg)
	}
	// Codes mangled by dictation or chat apps — a capitalized first letter,
	// a pasted newline, spaces or underscores for hyphens, no hyphens at
	// all, a trailing full stop — receive in canonical form. An existing
	// path by that exact name always wins, so no real file is shadowed;
	// --send forces send for one that doesn't exist yet.
	if c, ok := code.Normalize(arg); ok {
		if _, err := os.Lstat(arg); os.IsNotExist(err) {
			return startReceive(f, c)
		}
	}
	return runSend(f, []string{arg})
}

// stdinHasData reports whether stdin is a data source worth sending: a
// pipe or socket, or a non-empty regular file. A terminal, /dev/null (a
// character device), an empty file or a closed fd is not. A var so tests
// can swap it.
var stdinHasData = func() bool {
	st, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	switch m := st.Mode(); {
	case m&os.ModeCharDevice != 0:
		return false
	case m&(os.ModeNamedPipe|os.ModeSocket) != 0:
		return true
	case m.IsRegular():
		return st.Size() > 0
	}
	return false
}

// askForCode is bare `--receive`: the code comes from FSEND_CODE or a
// prompt, never argv, so other local users can't read it from `ps`.
func askForCode() (string, error) {
	if c := os.Getenv("FSEND_CODE"); c != "" {
		return c, nil
	}
	if !stdinIsTTY() {
		return "", fmt.Errorf("%w: --receive needs the code: pass it, set FSEND_CODE, or run in a terminal to be asked",
			fserrors.ErrUsage)
	}
	for range 3 {
		fmt.Fprint(os.Stderr, "  "+uxlog.Prompt("Code from the sender: "))
		line, eof := readLine(os.Stdin)
		if eof {
			fmt.Fprintln(os.Stderr)
			return "", fmt.Errorf("%w: no code entered", fserrors.ErrUsage)
		}
		if c, ok := code.Normalize(line); ok {
			return c, nil
		}
		if line != "" {
			fmt.Fprintf(os.Stderr, "  That isn't a code — codes look like %s.\n", uxlog.Code("abc-defg-jkm"))
		}
	}
	return "", fserrors.ErrInvalidCodeFormat
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
//
// FSEND_PASSWORD_FILE is the fallback when FSEND_PASSWORD is unset, so the
// secret can come from a mounted file instead of the environment.
func applyEnvFallbacks(f *flags, cmd *cobra.Command) error {
	bare := f.passArg == passPromptSentinel
	if !cmd.Flags().Changed("password") || bare {
		v, err := passwordFromEnv("FSEND_PASSWORD")
		if err != nil {
			return err
		}
		if v != "" {
			f.passArg = v
			// The user who typed bare --password expects a prompt; say why
			// none appears rather than silently pre-empting it.
			if bare && !f.quiet {
				fmt.Fprintln(os.Stderr, uxlog.Info(), "Using the password from the environment — skipping the prompt.")
			}
		}
	}
	return nil
}

// startReceive rejects send-only flags before handing off to runReceive,
// so `fsend <code> --exclude '*.log'` fails fast instead of silently
// ignoring the flag.
func startReceive(f *flags, c string) error {
	errorRole = "receiver"
	if len(f.excludes) > 0 {
		return errExcludeMisuse()
	}
	if f.preview {
		return fmt.Errorf("%w: --preview is a send-side flag and has no effect when receiving", fserrors.ErrUsage)
	}
	if f.as != "" {
		return fmt.Errorf("%w: --as is a send-side flag and has no effect when receiving", fserrors.ErrUsage)
	}
	// An explicit --receive (or FSEND_CODE) deserves the same tolerance
	// auto-detect has for dictated and chat-mangled codes — the more
	// explicit user must not get the worse E004.
	if n, ok := code.Normalize(c); ok {
		c = n
	}
	receiveResume = resumeCommand(f, c)
	return runReceive(f, c)
}

// receiveResume is the command that resumes this receive, for messages
// that tell the user how ("resume with: fsend abc-defg-jkm").
var receiveResume string

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
		fmt.Fprint(os.Stderr, "  Send this file, or receive with this code? [s/R] ")
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
