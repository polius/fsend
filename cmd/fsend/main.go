// Command fsend is the user-facing CLI for peer-to-peer file transfers.
//
// Dispatch follows PROJECT_SPEC.md "Dispatch rules":
//   - fsend                  → help
//   - fsend <code>           → receive (when <code> matches the code regex
//                              and no file with that name is in CWD)
//   - fsend <path>           → send
//   - fsend -                → send from stdin
//   - fsend --send / --receive force mode (skip auto-detect)
//
// v0.1.0 supports LAN-only operation (mDNS discovery + QUIC transfer).
// Rendezvous, ICE, and relay layers wire in later under the same CLI
// surface — no user-visible API changes when they land.
package main

import (
	"fmt"
	"os"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/uxlog"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		entry, _ := fserrors.Lookup(err)
		// Render the error to stderr without color for now (color is added
		// when we wire vbauerster/mpb + ANSI-aware UX polish).
		fmt.Fprintf(os.Stderr, "%s %s\n", marker("✗", "FAIL"), entry.Format())
		os.Exit(entry.Exit)
	}
}

// marker returns the unicode glyph when stderr is a TTY, the ASCII
// fallback otherwise. Caller passes both forms.
func marker(unicode, ascii string) string {
	if uxlog.IsTTY(os.Stderr) {
		return unicode
	}
	return ascii
}
