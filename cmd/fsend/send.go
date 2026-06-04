package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/polius/fsend/internal/code"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/landisc"
	"github.com/polius/fsend/internal/quicconn"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/version"
	"github.com/polius/fsend/internal/wire"
)

// runSend executes the send-side flow.
//
// v0.1.0 LAN-only path:
//  1. Walk the input paths.
//  2. Generate (or accept) a code.
//  3. Compute the deterministic LAN port from the code.
//  4. Bind a QUIC listener on that port.
//  5. Announce via mDNS.
//  6. Accept the first incoming QUIC connection.
//  7. Run the transfer protocol.
func runSend(f *flags, paths []string) error {
	if f.textArg != "" && len(paths) > 0 {
		return errors.New("--text cannot be combined with file arguments")
	}
	if f.textArg == "" && len(paths) == 0 {
		return errors.New("nothing to send (provide a file, a directory, or --text)")
	}

	items, kind, err := collectItems(f, paths)
	if err != nil {
		return err
	}

	c := f.codeArg
	if c == "" {
		c, err = code.Generate()
		if err != nil {
			return fmt.Errorf("generating code: %w", err)
		}
	} else {
		if err := code.Validate(c); err != nil {
			return fserrors.ErrInvalidCodeFormat
		}
	}

	port := landisc.PortForCode(c)
	listenAddr := ":" + strconv.Itoa(port)
	ln, err := quicconn.ListenAddr(listenAddr)
	if err != nil {
		// Port collision (rare given hashing). Surface a clear error.
		return fmt.Errorf("binding QUIC listener on %s: %w", listenAddr, err)
	}
	defer ln.Close()

	announceIP := landisc.PreferredLocalIP()
	mdnsConn, err := landisc.Announce(c, announceIP, port)
	if err != nil {
		return fmt.Errorf("publishing mDNS announce: %w", err)
	}
	defer mdnsConn.Close()

	hostname := f.hostname
	if hostname == "" {
		hostname, _ = os.Hostname()
	}

	// Render the artifact block.
	printSendArtifact(c, items, kind)

	// Listen for Ctrl-C.
	ctx, cancel := signalContext()
	defer cancel()

	res, err := ln.Accept(ctx)
	if err != nil {
		return fmt.Errorf("accepting QUIC: %w", err)
	}
	defer res.Close()

	fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Direct connection established (LAN)")

	sendErr := transfer.Send(ctx, &res.Streams, transfer.SendOptions{
		Items:         items,
		Hostname:      hostname,
		OS:            runtime.GOOS,
		ClientVersion: version.Version,
		TransferKind:  kind,
		Compress:      !f.noCompress,
	})
	if sendErr != nil {
		return sendErr
	}
	fmt.Fprintln(os.Stderr, marker("✓", "[OK]"), "Transfer complete")
	return nil
}

// collectItems resolves CLI args into the SourceItem list the wire
// protocol expects, plus the TransferKind discriminator.
//
// Handles four cases:
//   --text → synthetic SourceItem
//   "-"    → stdin (one synthetic SourceItem)
//   single path → file/directory walk
//   multiple paths → multi-file walk
func collectItems(f *flags, paths []string) ([]transfer.SourceItem, wire.TransferKind, error) {
	if f.textArg != "" {
		return synthesizeText(f.textArg), wire.TransferText, nil
	}
	if len(paths) == 1 && paths[0] == "-" {
		return synthesizeStdin(), wire.TransferStdin, nil
	}
	items, err := transfer.Walk(paths)
	if err != nil {
		return nil, 0, err
	}
	if len(paths) == 1 {
		// single path: file or directory?
		st, err := os.Stat(paths[0])
		if err == nil && st.IsDir() {
			return items, wire.TransferDirectory, nil
		}
		return items, wire.TransferSingleFile, nil
	}
	return items, wire.TransferMultiFile, nil
}

func synthesizeText(s string) []transfer.SourceItem {
	// A text item is delivered as a small "fsend-text-<rand>.txt" file.
	// For LAN MVP, hash is irrelevant (resume is disabled for synthetic
	// items); we leave Blake3Root zero — the receiver still verifies via
	// per-chunk hashes.
	name := "fsend-text-" + shortRand() + ".txt"
	return []transfer.SourceItem{
		{
			Info: wire.FileInfo{
				Index:        0,
				RelativePath: name,
				Size:         uint64(len(s)),
				Mode:         0o644,
				ModTime:      time.Now().UnixNano(),
				Resumable:    false,
			},
			Reader: byteReader(s),
		},
	}
}

func synthesizeStdin() []transfer.SourceItem {
	name := "fsend-stdin-" + shortRand()
	return []transfer.SourceItem{
		{
			Info: wire.FileInfo{
				Index:        0,
				RelativePath: name,
				Size:         0, // unknown
				Mode:         0o644,
				ModTime:      time.Now().UnixNano(),
				Resumable:    false,
			},
			Reader: os.Stdin,
		},
	}
}

// byteReader returns a simple io.Reader over the given string.
type stringReader struct {
	s   string
	off int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.off >= len(r.s) {
		return 0, errReadEOF
	}
	n := copy(p, r.s[r.off:])
	r.off += n
	return n, nil
}

var errReadEOF = errEOF{}

type errEOF struct{}

func (errEOF) Error() string { return "EOF" }

func byteReader(s string) *stringReader { return &stringReader{s: s} }

// shortRand returns an 8-char crypto-random alphanumeric string for
// synthetic filenames.
func shortRand() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	var b [8]byte
	if _, err := rand_Read(b[:]); err != nil {
		return "00000000"
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b[:])
}

// rand_Read is split out to keep cmd/fsend's import list visible at the
// top of the file. It returns crypto/rand.Read.
func rand_Read(b []byte) (int, error) {
	return randReadHelper(b)
}

// printSendArtifact renders the "code + receive command" block on stderr
// per PROJECT_SPEC.md "Send-side terminal UX" state 1.
func printSendArtifact(c string, items []transfer.SourceItem, kind wire.TransferKind) {
	fmt.Fprintln(os.Stderr)
	switch kind {
	case wire.TransferSingleFile:
		fmt.Fprintf(os.Stderr, "  Sending %s  (%s)\n", items[0].Info.RelativePath, humanBytes(int64(items[0].Info.Size)))
	case wire.TransferDirectory:
		var total uint64
		for _, it := range items {
			total += it.Info.Size
		}
		fmt.Fprintf(os.Stderr, "  Sending %s/  (%d files, %s)\n", items[0].Info.RelativePath, len(items)-1, humanBytes(int64(total)))
	case wire.TransferMultiFile:
		fmt.Fprintf(os.Stderr, "  Sending %d items\n", len(items))
	case wire.TransferText:
		fmt.Fprintln(os.Stderr, "  Sending text")
	case wire.TransferStdin:
		fmt.Fprintln(os.Stderr, "  Sending from stdin")
	}
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  ─────────────────────────────────────────────")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintf(os.Stderr, "      %s\n", c)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  On the other machine, run:")
	fmt.Fprintf(os.Stderr, "      fsend %s\n", c)
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "  ─────────────────────────────────────────────")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, marker("⠋", "[*]"), "Waiting for receiver…")
}

// humanBytes renders a byte count in compact form.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// signalContext wires Ctrl-C / SIGTERM to ctx cancellation so transfers
// can be cleanly aborted.
func signalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		cancel()
	}()
	return ctx, cancel
}

// unused noise suppression
var _ = net.IPv4
