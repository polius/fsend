package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/version"
	"github.com/polius/fsend/internal/wire"
)

// receiverUI bundles the receive-side engine callbacks (accept prompt,
// progress bar, overwrite confirm) with the state they share: the
// sender's HELLO, the finalized file paths, and the byte counter the
// summary reports. One instance serves a whole receive, including
// retries.
//
// The progress bar is created lazily on the first byte rather than at
// accept time — the password handshake may still need to prompt on
// stderr, and a rejected transfer must leave no half-rendered bar.
type receiverUI struct {
	ctx      context.Context
	f        *flags
	outDir   string // absolute; "" in sink mode
	sink     bool
	pathInfo connpath.Info

	mu             sync.Mutex
	hello          *wire.SenderHello
	files          []string
	prev           map[uint32]uint64
	total          int64 // bytes received this run (excludes resumed prefixes)
	skipped        int64 // resumed prefixes already on disk; see onResume
	bar            *uxlog.Progress
	firstByte      time.Time // latched on first progress byte; see finishReceive
	totalBytesHint int64
	streamingTotal bool // HELLO carried TotalBytes=0 (piped stdin)
	preApproved    bool // accept prompt already disclosed the overwrite

	closeOnce sync.Once
}

func newReceiverUI(ctx context.Context, f *flags, outDir string, sink bool, pathInfo connpath.Info) *receiverUI {
	return &receiverUI{
		ctx: ctx, f: f, outDir: outDir, sink: sink, pathInfo: pathInfo,
		prev: make(map[uint32]uint64),
	}
}

// resolveOutDir resolves --out: "-" selects sink (stdout) mode, ""
// defaults to the CWD. Directories come back absolute so the prompt and
// the summary show a stable path regardless of how the user spelled it.
// An explicit --out must already exist — failing fast here beats
// accepting a transfer that is doomed at write time.
func resolveOutDir(f *flags) (dir string, sink bool, err error) {
	if f.outDir == "-" {
		return "", true, nil
	}
	dir = f.outDir
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return "", false, err
		}
	}
	if abs, absErr := filepath.Abs(dir); absErr == nil {
		dir = abs
	}
	if f.outDir != "" {
		st, statErr := os.Stat(dir)
		switch {
		case statErr != nil:
			return "", false, fmt.Errorf("%w: --out directory does not exist: %s", fserrors.ErrUsage, dir)
		case !st.IsDir():
			return "", false, fmt.Errorf("%w: --out is not a directory: %s", fserrors.ErrUsage, dir)
		}
	}
	return dir, false, nil
}

// recvOptions assembles the engine options around this UI's callbacks.
func (ui *receiverUI) recvOptions(hostname string) transfer.RecvOptions {
	o := transfer.RecvOptions{
		Hostname:      hostname,
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TargetDir:     ui.outDir,
		Overwrite:     ui.f.overwrite,
		Accept:        ui.accept,
		Password:      ui.f.passArg,
		PromptPass:    receiverPasswordPrompt(ui.ctx, ui.f),
		ProgressFn:    ui.progress,
		OnResume:      ui.onResume,
		OnFileDone:    ui.onFileDone,
	}
	if ui.sink {
		o.Sink = os.Stdout
	}
	// Under --quiet or --yes the confirm stays nil: the engine then
	// rejects collisions with E013 instead of silently overwriting or
	// blocking on a prompt nobody will answer.
	if !ui.f.quiet && !ui.f.yes {
		o.ConfirmOverwrite = ui.confirmOverwrite
	}
	return o
}

// accept records the HELLO, runs the prompt, and pre-approves the
// per-file overwrite confirm when the prompt already disclosed the
// collision — the user answers one question, not two.
func (ui *receiverUI) accept(h wire.SenderHello) bool {
	ui.mu.Lock()
	cp := h
	ui.hello = &cp
	ui.totalBytesHint = int64(h.TotalBytes)
	ui.streamingTotal = h.TotalBytes == 0
	ui.mu.Unlock()

	if !ui.promptAccept(h) {
		return false
	}
	if !ui.sink && h.TransferKind == wire.TransferSingleFile && !ui.f.overwrite {
		target := filepath.Join(ui.outDir, h.DisplayName)
		if st, err := os.Stat(target); err == nil && !st.IsDir() {
			ui.mu.Lock()
			ui.preApproved = true
			ui.mu.Unlock()
		}
	}
	return true
}

// promptAccept renders the incoming-transfer block on stderr and asks
// whether to receive:
//
//	Incoming from <peer>  [· via relay]
//
//	    <artifact>  ·  <size>  [🔒 password]
//	    [⚠ already in <dir> · will overwrite if you accept]
//
//	Save to <path>/? [Y/n]
//
// The question adapts to the destination: "Write to stdout?" in sink
// mode, "Accept?" for text (which prints rather than saves).
// Unrecognized input re-prompts instead of defaulting — a stray typo
// next to 'n' must not accept a transfer.
func (ui *receiverUI) promptAccept(h wire.SenderHello) bool {
	f := ui.f
	if f.quiet {
		return f.yes
	}
	peer := sanitizeRemote(h.Hostname)
	pathChip := ""
	if ui.pathInfo.Kind == connpath.KindRelay {
		pathChip = uxlog.Dim("  ·  via relay")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  Incoming from %s%s\n", peer, pathChip)
	fmt.Fprintln(os.Stderr)
	renderArtifact(os.Stderr, h, ui.outDir, f)
	fmt.Fprintln(os.Stderr)
	if f.yes {
		fmt.Fprintln(os.Stderr, uxlog.Check(), "Accepting (--yes)")
		return true
	}
	question := "Save to " + saveTargetLabel(ui.outDir) + "?"
	switch {
	case ui.sink:
		question = "Write to stdout?"
	case h.TransferKind == wire.TransferText:
		question = "Accept?"
	}
	for {
		fmt.Fprintf(os.Stderr, "  %s [Y/n] ", question)
		// Decline on Ctrl-C; the caller maps a cancelled ctx to E026.
		line, eof, ok := readLineCtx(ui.ctx)
		if !ok {
			return false
		}
		// Closed stdin must not take the interactive yes-default: a cron
		// job or `fsend <code> </dev/null` would silently consent to
		// writing files (and pre-approve an overwrite).
		if eof {
			fmt.Fprintf(os.Stderr, "\n%s No input to answer the prompt — declining. Pass --yes to accept automatically.\n", uxlog.Info())
			return false
		}
		switch line {
		case "", "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(os.Stderr, "  Please answer y or n.")
	}
}

func (ui *receiverUI) confirmOverwrite(relPath string, existing int64, incoming uint64) bool {
	ui.mu.Lock()
	pre := ui.preApproved
	ui.mu.Unlock()
	if pre {
		return true
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "  %s already exists  ·  local %s  ·  incoming %s\n",
		relPath, uxlog.HumanBytes(existing), uxlog.HumanBytes(int64(incoming)))
	fmt.Fprint(os.Stderr, "  Overwrite? [y/N] ")
	line, _, ok := readLineCtx(ui.ctx)
	if !ok {
		return false
	}
	switch line {
	case "y", "yes":
		return true
	default:
		return false
	}
}

// onResume announces a resumed file and fast-forwards the bookkeeping
// past the on-disk prefix: the bar starts at the resume point, but the
// prefix is booked as skipped, not received — so the summary's rate
// reflects only bytes that actually moved. On a mid-run retry prev
// already covers the prefix (we received those bytes ourselves).
func (ui *receiverUI) onResume(fileIndex uint32, offset, total uint64) {
	ui.mu.Lock()
	if offset > ui.prev[fileIndex] {
		d := int64(offset - ui.prev[fileIndex])
		ui.prev[fileIndex] = offset
		ui.skipped += d
		if ui.bar == nil && !ui.f.quiet {
			ui.bar = uxlog.New(ui.totalBytesHint)
		}
		ui.bar.Add(d)
	}
	quiet := ui.f.quiet
	ui.mu.Unlock()
	if !quiet {
		uxlog.Println(fmt.Sprintf("  %s %s", uxlog.Info(), resumeNotice(offset, total)))
	}
}

func (ui *receiverUI) progress(fileIndex uint32, bytesWritten uint64) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	if ui.firstByte.IsZero() {
		ui.firstByte = time.Now()
	}
	if ui.bar == nil && !ui.f.quiet {
		ui.bar = uxlog.New(ui.totalBytesHint)
	}
	d := bytesWritten - ui.prev[fileIndex]
	ui.prev[fileIndex] = bytesWritten
	ui.total += int64(d)
	ui.bar.Add(int64(d)) // nil-safe under --quiet
}

func (ui *receiverUI) onFileDone(path string) {
	ui.mu.Lock()
	ui.files = append(ui.files, path)
	ui.mu.Unlock()
}

// bytes returns the landed total (received + resumed prefixes) and the
// bytes that actually moved this run, for the summary line.
func (ui *receiverUI) bytes() (total, moved int64) {
	ui.mu.Lock()
	defer ui.mu.Unlock()
	return ui.total + ui.skipped, ui.total
}

// close flushes the progress bar. Idempotent: callers flush explicitly
// before the summary and keep a deferred call as the safety net.
func (ui *receiverUI) close() {
	ui.closeOnce.Do(func() {
		ui.mu.Lock()
		bar, total, streaming := ui.bar, ui.total, ui.streamingTotal
		ui.mu.Unlock()
		if bar == nil {
			return
		}
		// Streaming transfers latch the bar to the real total so the
		// terminal frame reads "done" instead of "aborted".
		if streaming && total > 0 {
			bar.SetTotal(total, true)
		}
		bar.Done()
	})
}

// finishReceive runs the post-transfer epilogue: flush the bar, print a
// received --text payload to stdout (text is a message, not a download),
// and render the summary.
func finishReceive(f *flags, ui *receiverUI, elapsed time.Duration) error {
	ui.close()

	ui.mu.Lock()
	h := ui.hello
	files := append([]string(nil), ui.files...)
	firstByte := ui.firstByte
	ui.mu.Unlock()

	// Time from the first byte: the caller's window includes the accept
	// prompt, and a 20 s deliberation would read as a glacial transfer.
	if !firstByte.IsZero() {
		elapsed = time.Since(firstByte)
	}

	if h != nil && h.TransferKind == wire.TransferText && !ui.sink {
		if err := printTextPayload(files); err != nil {
			return err
		}
		total, moved := ui.bytes()
		printRecvSummary(f, "Received text", total, moved, elapsed, ui.pathInfo)
		return nil
	}
	total, moved := ui.bytes()
	printRecvSummary(f, ui.headline(h, files), total, moved, elapsed, ui.pathInfo)
	return nil
}

// printTextPayload writes the received text to stdout and removes the
// carrier file. Terminal output gains a trailing newline so the shell
// prompt doesn't glue to the payload; piped output stays byte-exact.
func printTextPayload(files []string) error {
	if len(files) == 0 {
		return nil
	}
	b, err := os.ReadFile(files[0])
	if err != nil {
		return fmt.Errorf("%w: text payload: %v", fserrors.ErrReadFailed, err)
	}
	_ = os.Remove(files[0])
	if _, err := os.Stdout.Write(b); err != nil {
		return err
	}
	if len(b) > 0 && b[len(b)-1] != '\n' && term.IsTerminal(int(os.Stdout.Fd())) {
		fmt.Println()
	}
	return nil
}

// headline names what landed where for the summary line: "Saved
// hello.bin to ~/dir", "Saved 3 files to ~/dir", or bare "Received" in
// sink mode. The piped-stdin name is only knowable from the finalized
// path (the sender generates it), hence the files fallback.
func (ui *receiverUI) headline(h *wire.SenderHello, files []string) string {
	if ui.sink {
		return "Received"
	}
	name := ""
	if h != nil {
		switch h.TransferKind {
		case wire.TransferSingleFile, wire.TransferDirectory:
			name = sanitizeForDisplay(h.DisplayName, 128)
		case wire.TransferMultiFile:
			name = fmt.Sprintf("%d files", h.TotalFiles)
		case wire.TransferStdin:
			if len(files) > 0 {
				name = filepath.Base(files[0])
			}
		}
	}
	dest := displayPath(ui.outDir)
	if name == "" {
		return "Saved to " + dest
	}
	return "Saved " + name + " to " + dest
}

// printRecvSummary renders the post-transfer success line. headline is
// the fully-formed "Saved X to Y" / "Received" clause; the parts that
// follow scan size → time → route. On a resumed transfer moved < total
// and the size gains a "(X received)" clause. Suppressed under --quiet.
func printRecvSummary(f *flags, headline string, total, moved int64, elapsed time.Duration, path connpath.Info) {
	if f.quiet {
		return
	}
	if headline == "" {
		headline = "Received"
	}
	parts := summaryParts(total, moved, "received", elapsed, path)
	fmt.Fprintf(os.Stderr, "%s %s  ·  %s\n", uxlog.Check(), headline, strings.Join(parts, "  ·  "))
	printUpdateNotice(f)
}
