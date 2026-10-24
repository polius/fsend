package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/uxlog"
)

// stdinIsTTY reports whether stdin is an interactive terminal. A var (not
// a plain call) so it can be swapped out in tests without faking fds.
var stdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// readPasswordHidden prompts on stderr and reads a single line from the
// controlling terminal without echo. Falls back to plain stdin when
// stdin is not a TTY (e.g. inside a pipeline) — callers that need
// true no-echo should require the env var instead.
//
// Empty input gets one re-prompt with a hint, then surfaces as a usage
// error (mapped to E024) so the user sees a clean message instead of
// the E099 catchall.
func readPasswordHidden(ctx context.Context, prompt string) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		pw, err := readPasswordOnce(ctx, prompt)
		if err != nil {
			// EOF on a piped stdin (or a closed terminal) means the
			// caller can't supply input — surface as a usage error,
			// not the E099 catchall.
			if errors.Is(err, io.EOF) {
				return "", fmt.Errorf("%w: no password supplied (stdin closed)", fserrors.ErrUsage)
			}
			return "", err
		}
		if pw != "" {
			return pw, nil
		}
		if attempt == 0 {
			fmt.Fprintln(os.Stderr, "  Password cannot be empty — try again.")
		}
	}
	return "", fmt.Errorf("%w: password cannot be empty", fserrors.ErrUsage)
}

// readPasswordHiddenCtx is readPasswordHidden that also aborts on ctx
// cancellation (Ctrl-C at the prompt), returning context.Canceled. The
// abandoned ReadPassword goroutine cannot run its own deferred terminal
// restore, so we capture the terminal state up front and restore it here.
func readPasswordHiddenCtx(ctx context.Context, prompt string, quiet bool) (string, error) {
	fd := int(os.Stdin.Fd())
	var oldState *term.State
	if term.IsTerminal(fd) {
		oldState, _ = term.GetState(fd)
	}
	type result struct {
		pw  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		pw, err := readPasswordHidden(ctx, prompt)
		ch <- result{pw, err}
	}()
	select {
	case <-ctx.Done():
		if oldState != nil {
			_ = term.Restore(fd, oldState)
		}
		// signalContext's Cancelling notice already broke the prompt line;
		// another newline here showed up as a stray blank line. --quiet
		// suppresses that notice, so break the line ourselves then.
		if quiet {
			fmt.Fprintln(os.Stderr)
		}
		return "", context.Canceled
	case r := <-ch:
		return r.pw, r.err
	}
}

// resolvePassword expands the bare --password sentinel into a concrete
// password. No-op when --password wasn't given bare.
//
// Sender side: shows a freshly-generated four-word suggestion the user can
// accept with <enter> (or override by typing one). The suggestion is
// echoed in cleartext — the sender needs to read it back to share with
// the receiver, and the input it shadows is shown in plaintext anyway.
//
// Receiver side: hidden no-echo prompt. The receiver must type whatever
// the sender configured, so a "random default" makes no sense; we just
// reuse the standard hidden-password reader.
//
// ctx must come from signalContext(): once the handler is installed,
// SIGINT only cancels the context, so a prompt that doesn't select on
// ctx would leave Ctrl-C dead — and for the hidden prompt, the terminal
// stuck with echo off.
func resolvePassword(ctx context.Context, f *flags, sender bool) error {
	if f.passArg != passPromptSentinel {
		return nil
	}
	// Bare --password needs a TTY: the sender prompt echoes a suggestion the
	// user accepts with Enter; the receiver prompt reads no-echo. Either
	// way, doing it on a piped stdin reads the file's first line as the
	// password and breaks the transfer. FSEND_PASSWORD is the documented
	// non-interactive path.
	if !stdinIsTTY() {
		return fmt.Errorf("%w: bare --password needs a terminal; use --password=<value> or FSEND_PASSWORD", fserrors.ErrUsage)
	}
	if sender {
		pw, err := promptPasswordWithSuggestionCtx(ctx, f.quiet)
		if err != nil {
			return err
		}
		f.passArg = pw
		return nil
	}
	pw, err := readPasswordHiddenCtx(ctx, "  "+uxlog.Prompt("Enter the password the sender shared: "), f.quiet)
	if err != nil {
		return err
	}
	f.passArg = pw
	return nil
}

// promptPasswordWithSuggestionCtx is promptPasswordWithSuggestion that
// aborts on ctx cancellation (Ctrl-C at the prompt). The prompt echoes
// normally, so unlike the hidden reader there is no terminal state to
// restore — just move off the prompt line.
func promptPasswordWithSuggestionCtx(ctx context.Context, quiet bool) (string, error) {
	type result struct {
		pw  string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		pw, err := promptPasswordWithSuggestion(stdinReader())
		ch <- result{pw, err}
	}()
	select {
	case <-ctx.Done():
		// As in readPasswordHiddenCtx: signalContext already broke the
		// line unless --quiet suppressed its notice.
		if quiet {
			fmt.Fprintln(os.Stderr)
		}
		return "", context.Canceled
	case r := <-ch:
		return r.pw, r.err
	}
}

// promptPasswordWithSuggestion offers a freshly-generated random password
// as the default. The suggestion is printed visibly (the sender needs to
// see it to share it out-of-band) and a bare <enter> accepts it. Any
// non-empty typed input replaces the suggestion.
func promptPasswordWithSuggestion(br *bufio.Reader) (string, error) {
	suggested, err := generatePassphrase(passphraseWords)
	if err != nil {
		return "", fmt.Errorf("generating suggested password: %w", err)
	}
	fmt.Fprintf(os.Stderr, "  Suggested password: %s\n", suggested)
	fmt.Fprint(os.Stderr, "  "+uxlog.Prompt("Press Enter to use it, or type your own: "))

	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		// EOF on a piped stdin with no data: the caller can't interact.
		// Be explicit rather than silently accepting the suggested
		// value — a script that pipes nothing into fsend would never
		// know what password was chosen.
		if errors.Is(err, io.EOF) {
			return "", fmt.Errorf("%w: no password supplied (stdin closed)", fserrors.ErrUsage)
		}
		return "", err
	}
	// Trim only the line terminator: every other password path (hidden
	// read, --password=VALUE, FSEND_PASSWORD) takes the value verbatim,
	// so a trailing space typed here must stay part of the secret too.
	typed := strings.TrimRight(line, "\r\n")
	if typed == "" {
		return suggested, nil
	}
	return typed, nil
}

// passphraseWords is how many words a suggested password has. Four from
// the 512-word list is 36 bits: far beyond what the bounded online
// guessing a password gets (CPace leaves nothing to attack offline).
const passphraseWords = 4

// generatePassphrase returns n random words from passphraseList joined by
// hyphens ("maple-river-stone-candle"): easy to read aloud and type,
// unlike a mixed-case string. crypto/rand.Int gives unbiased sampling —
// same pattern as internal/code.Generate.
func generatePassphrase(n int) (string, error) {
	words := strings.Fields(passphraseList)
	max := big.NewInt(int64(len(words)))
	out := make([]string, n)
	for i := range out {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = words[idx.Int64()]
	}
	return strings.Join(out, "-"), nil
}

// passphraseList holds short, common, distinct-sounding English words:
// lowercase, no homophones, nothing easily misheard over a call.
//
//nolint:gosec // G101: a public word list, not a credential
const passphraseList = `
able acid acorn actor adapt admit adult agent agree ahead aisle alarm album alert alive allow alpha amber ample angle
ankle apple april apron arena armor arrow atlas attic audio aunt award awake axis bacon badge bagel baker bamboo banjo
barn basil basin batch beach beard beast beaver begin bench berry bike bingo birch bison blade blank blast blend bliss
block bloom blues board boat bonus boost booth bound boxer brain brass brave bread brick bride brief brisk broom brush
bucket buddy budget buffet bugle bunny burst butter cabin cable cactus camel camera canal candle candy canoe canvas
cargo carpet carrot castle cedar chalk charm chart cheese cherry chess chief chill chimney cider cinema circus civic
claim clam clay clerk cliff climb clock cloud clover coach coast cobra cocoa comet comic coral cotton couch cougar
cousin cowboy crab craft crane crater crayon cream creek cricket crisp crown cruise crumb crystal cube cupcake curtain
cycle daisy dance dawn delta denim desert detail diary dinner disco dock dolphin donkey donut dragon drama dream drift
drill drum duck dune eagle early earth easel echo elbow elder elk ember empty energy engine enjoy entry epic equal
erupt essay exact exile extra fabric fairy falcon family fancy farm feast fence ferry fever fiber fiddle field fig
film final finch fjord flag flame flash fleet flint float flock flute focus foggy forest fossil fox frame fresh frog
frost fruit fudge galaxy garden garlic gecko gentle giant ginger giraffe glacier glad glass globe glove goat golden
goose gorilla grain grape gravel green grill grove guitar gulf habit hammer hamster harbor harvest hawk hazel
heart hedge helmet hero hiking hippo hobby honey hood hotel humble hunter husky igloo index iris island ivory jacket
jaguar jam jasmine jelly jewel jigsaw jockey joke jolly journal judge juice jungle kayak kettle kidney kitten kiwi
koala ladder lagoon lake lamp lantern laptop large laser lava lawn lemon level lily limit linen lion lizard llama
lobster locket lodge lotus lucky lunar lunch magnet mango maple marble market marsh mask meadow melon memory mentor
metal meteor middle mimic mint mirror mitten mocha model monkey moose mosaic moss motor mountain muffin museum music
napkin narrow nature nectar needle nest nickel noble noodle north novel nugget oasis ocean olive omega onion opera
orbit orchid organ otter outfit oven owl oyster paddle palace panda panther paper parade parrot pasta peach peanut
pebble pencil penguin pepper piano pickle pigeon pillow pilot pine pirate pizza planet plaza plum pocket
polar pony poppy potato prairie prism pumpkin puppy puzzle quartz quest quilt rabbit radar radio raft rainbow
raisin ranch raven razor recipe reef relic rhino ribbon ridge river robin rocket rodeo royal ruby rumble
saddle safari salad salmon sandal satin sauce scarf scout season secret shadow shark shelf shield silver siren skate
sketch sleigh slogan smile snack snail socket sofa solar sonic spark sphinx spider spinach sponge spruce squid stable
stamp statue steam stereo stone storm studio sugar summit sunny surf swan sweater symbol syrup table tactic tango
teapot tennis tiger timber toast tomato topaz torch tower tractor trail tulip tundra turkey turtle tuxedo umbrella
unicorn urban valley vanilla velvet venture violin visit vivid volcano voyage waffle walnut walrus wander wave
wheat whale whistle willow window winter wizard wolf wombat yacht yodel yogurt zebra zenith zigzag zipper
`

// readPasswordOnce performs a single hidden-input read. Returns "" with
// no error on empty input so the caller can re-prompt without conflating
// "empty" with "I/O failure".
func readPasswordOnce(ctx context.Context, prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		// Last gate before raw mode: past this point the read owns the
		// terminal, and a cancel racing it would leave echo off at exit.
		if err := ctx.Err(); err != nil {
			return "", err
		}
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		return string(b), nil
	}
	// Non-TTY: read one line (no echo possible). Use the package-level
	// shared bufio.Reader on os.Stdin so an earlier readLine() call
	// hasn't left buffered bytes in a now-discarded reader.
	line, err := stdinReader().ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	// Trim trailing newline / CRLF.
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line, nil
}
