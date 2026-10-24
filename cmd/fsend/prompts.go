package main

import (
	"bufio"
	"context"
	"io"
	"os"
	"strings"
	"sync"
)

// readLine reads one line from r, trims trailing CR/LF, lowercases it,
// and returns the result. eof is true when the read produced no input at
// all (closed stdin, /dev/null, exhausted pipe) — callers that treat an
// empty line as "take the default" must NOT do so on eof: nobody is
// answering, and a default that consents to something is a footgun.
// A read error that still yielded bytes is a valid answer line.
//
// When r is os.Stdin, the package-level shared bufio.Reader is used so
// bytes are not lost between successive prompts (see stdinReader).
func readLine(r io.Reader) (line string, eof bool) {
	br := stdinReader()
	if r != os.Stdin {
		br = bufio.NewReader(r)
	}
	return readLineFrom(br)
}

// readLineFrom is readLine for callers that loop over one prompt (and so
// must hold a single buffered reader to avoid losing bytes between reads).
func readLineFrom(br *bufio.Reader) (line string, eof bool) {
	raw, err := br.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(raw)), err != nil && raw == ""
}

// readLineCtx reads one line from os.Stdin but returns ok=false if ctx is
// cancelled first — e.g. Ctrl-C, or the sender's connection dropping
// while the prompt waits. An abandoned read stays pending and the next
// prompt picks it up, so two reads never race for the same line.
func readLineCtx(ctx context.Context) (line string, eof, ok bool) {
	pendingMu.Lock()
	if pendingLine == nil {
		ch := make(chan lineResult, 1)
		go func() {
			l, e := readLine(os.Stdin)
			ch <- lineResult{l, e}
		}()
		pendingLine = ch
	}
	ch := pendingLine
	pendingMu.Unlock()
	select {
	case <-ctx.Done():
		return "", false, false
	case r := <-ch:
		pendingMu.Lock()
		pendingLine = nil
		pendingMu.Unlock()
		return r.line, r.eof, true
	}
}

type lineResult struct {
	line string
	eof  bool
}

var (
	pendingMu   sync.Mutex
	pendingLine chan lineResult
)

// stdinReader returns the process-wide bufio.Reader over os.Stdin.
//
// Successive prompts (the "Save to ...?" line and the password line, in
// the password-protected receive flow) both need to read one line from
// stdin. Constructing a fresh bufio.Reader per prompt would let the
// reader buffer past its terminating newline and then discard the
// remainder when it goes out of scope, eating the next prompt's input
// — a sharp edge that bit the password flow when stdin was piped.
//
// A single shared reader keeps the read pointer monotonically advancing
// across prompts. Safe for the CLI's single-threaded prompt sequence;
// the sync.Once just guards construction.
func stdinReader() *bufio.Reader {
	stdinReaderOnce.Do(func() {
		stdinReaderShared = bufio.NewReader(os.Stdin)
	})
	return stdinReaderShared
}

var (
	stdinReaderShared *bufio.Reader
	stdinReaderOnce   sync.Once
)
