package main

import (
	"bufio"
	"io"
	"strings"
)

// readLine reads one line from r, trims trailing CR/LF, lowercases it,
// and returns the result. EOF and read errors both collapse to the empty
// string so the caller sees "default" rather than a propagated read
// error — empty input is a valid prompt response, not a failure mode.
func readLine(r io.Reader) string {
	br := bufio.NewReader(r)
	line, _ := br.ReadString('\n')
	return strings.ToLower(strings.TrimSpace(line))
}
