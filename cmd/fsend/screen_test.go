package main

import (
	"bufio"
	"fmt"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/uxlog"
	"github.com/polius/fsend/internal/wire"
)

// D1: the install-and-receive block names the PowerShell line right under
// the sh one, each alone on its line in the list column. Asserted on a
// pipe: no colour, and no terminal width to wrap at.
func TestPrintSendArtifact_InstallHintBothShells(t *testing.T) {
	plan := &sendPlan{mode: wire.ModeFiles, totalFiles: 1, totalBytes: 7, label: "a.txt"}
	got := captureStderr(t, func() {
		sp := printSendArtifact(&flags{}, "abc-defg-hij", plan, true)
		sp.Stop()
	})
	want := "\n" +
		"  No fsend there yet? This installs it and receives:\n" +
		"    curl -fsSL https://getfsend.alzina.dev | sh -s -- abc-defg-hij\n" +
		"    iex \"& {$(irm https://getfsend.alzina.dev/windows)} -Code abc-defg-hij\"\n" +
		"\n"
	if !strings.Contains(got, want) {
		t.Errorf("install block =\n%s\nwant it to contain\n%s", got, want)
	}
	got = captureStderr(t, func() {
		sp := printSendArtifact(&flags{}, "abc-defg-hij", plan, false)
		sp.Stop()
	})
	if strings.Contains(got, "irm") || strings.Contains(got, "curl") {
		t.Errorf("a server of its own must get neither line:\n%s", got)
	}
}

// Both install lines fit 80 columns under the list indent, whatever the
// code (every code is 12 characters).
func TestInstallCommandsFit80Columns(t *testing.T) {
	const code = "abc-defg-jkm"
	for name, cmd := range map[string]string{"sh": installCommand(code), "PowerShell": installCommandPowerShell(code)} {
		line := uxlog.ItemRow(cmd)
		if n := uxlog.Width(line); n > 79 {
			t.Errorf("%s line is %d columns: %q", name, n, line)
		}
		if strings.ContainsAny(cmd, "\n\r") {
			t.Errorf("%s line is not one line: %q", name, cmd)
		}
		if !strings.Contains(cmd, code) {
			t.Errorf("%s line lost the code: %q", name, cmd)
		}
	}
	// The form scripts/install.ps1 documents for -Code (and
	// scripts/smoke-install.ps1 runs): the script as a block, then -Code.
	if got, want := installCommandPowerShell(code), `iex "& {$(irm https://getfsend.alzina.dev/windows)} -Code abc-defg-jkm"`; got != want {
		t.Errorf("installCommandPowerShell = %q, want %q", got, want)
	}
}

// U8: the skipped-files list never ends "… and 1 more": that line would
// take the row it saves. One over the limit prints whole; two over
// collapses to the limit and "… and 2 more".
func TestPrintSkippedList_NeverHidesExactlyOneRow(t *testing.T) {
	rows := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("dir/file%02d.txt — permission denied", i)
		}
		return out
	}
	for _, c := range []struct {
		n, wantRows int
		wantMore    string
	}{
		{1, 1, ""},
		{skippedListRows, skippedListRows, ""},
		{skippedListRows + 1, skippedListRows + 1, ""},
		{skippedListRows + 2, skippedListRows, "    … and 2 more\n"},
		{skippedListRows + 30, skippedListRows, "    … and 30 more\n"},
	} {
		got := captureStderr(t, func() { printSkippedList(&flags{}, "could not be read", rows(c.n), 0) })
		if n := strings.Count(got, "    dir/file"); n != c.wantRows {
			t.Errorf("%d skipped: %d rows listed, want %d:\n%s", c.n, n, c.wantRows, got)
		}
		if strings.Contains(got, "and 1 more") {
			t.Errorf("%d skipped: a row is hidden behind \"… and 1 more\":\n%s", c.n, got)
		}
		if c.wantMore == "" && strings.Contains(got, "more") {
			t.Errorf("%d skipped: unexpected remainder line:\n%s", c.n, got)
		}
		if c.wantMore != "" && !strings.HasSuffix(got, c.wantMore) {
			t.Errorf("%d skipped: want it to end %q:\n%s", c.n, c.wantMore, got)
		}
		verb := "were"
		if c.n == 1 {
			verb = "was"
		}
		if head := fmt.Sprintf("[!] %s could not be read and %s skipped:\n", uxlog.CountNoun(c.n, "file"), verb); !strings.HasPrefix(got, head) {
			t.Errorf("%d skipped: headline = %q, want %q", c.n, strings.SplitN(got, "\n", 2)[0], head)
		}
	}
}

// The receiver names only so many of the files it refused and counts the
// rest: the headline counts them all, and the closing line stands for the
// rows that were cut and the files that never had one.
func TestPrintSkippedList_CountsFilesWithoutARow(t *testing.T) {
	rows := make([]string, skippedListRows+5)
	for i := range rows {
		rows[i] = fmt.Sprintf("dir/file%02d.txt — name too long", i)
	}
	got := captureStderr(t, func() { printSkippedList(&flags{}, "", rows, 20) })
	if head := fmt.Sprintf("[!] %s were skipped:\n", uxlog.CountNoun(len(rows)+20, "file")); !strings.HasPrefix(got, head) {
		t.Errorf("headline = %q, want %q", strings.SplitN(got, "\n", 2)[0], head)
	}
	if n := strings.Count(got, "    dir/file"); n != skippedListRows {
		t.Errorf("%d rows listed, want %d:\n%s", n, skippedListRows, got)
	}
	if want := "    … and 25 more\n"; !strings.HasSuffix(got, want) {
		t.Errorf("want it to end %q:\n%s", want, got)
	}

	// Every row fits: only the unnamed files are behind the closing line.
	got = captureStderr(t, func() { printSkippedList(&flags{}, "", rows[:3], 2) })
	if n := strings.Count(got, "    dir/file"); n != 3 || !strings.HasSuffix(got, "    … and 2 more\n") {
		t.Errorf("3 rows and 2 unnamed:\n%s", got)
	}
}

// V7: the prompts outside the main flow lead with the ? gutter mark (the
// "[?]" marker on a pipe), the question from column 2, the options two
// spaces after it; what surrounds them sits in column 2.
func TestPromptsOutsideTheMainFlow(t *testing.T) {
	// The code-or-file question: a typo re-asks, "s" answers.
	got := captureStderr(t, func() {
		_, _ = askCodeOrPath(bufio.NewReader(strings.NewReader("x\ns\n")), "abc-defg-jkm")
	})
	want := "\n" +
		"  \"abc-defg-jkm\" matches a code AND is a local file.\n" +
		"[?] Send this file, or receive with this code?  [s/R] " +
		"  Please answer s or r.\n" +
		"[?] Send this file, or receive with this code?  [s/R] "
	if got != want {
		t.Errorf("code-or-file prompt =\n%q\nwant\n%q", got, want)
	}
	// The sender's password suggestion.
	got = captureStderr(t, func() {
		_, _ = promptPasswordWithSuggestion(bufio.NewReader(strings.NewReader("\n")))
	})
	lines := strings.SplitN(got, "\n", 2)
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "  Suggested password: ") ||
		lines[1] != "[?] Press Enter to use it, or type your own: " {
		t.Errorf("password suggestion prompt = %q", got)
	}
}

// U2: the file list pads its name column in display columns, so the sizes
// line up under names in a wide script (two columns a character).
func TestRenderPreview_AlignsWideNames(t *testing.T) {
	got := render([]previewItem{
		{name: "album/富士山の夜明け.jpeg", size: 12_000_000},
		{name: "album/notes.txt", size: 9_000_000},
		{name: "album/회의록.docx", size: 500},
	})
	var starts []int
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		i := strings.LastIndex(line, uxlog.Gap)
		if i < 0 {
			t.Fatalf("row without a size column: %q", line)
		}
		// Sizes are right-aligned: the row's full width is what must match.
		starts = append(starts, uxlog.Width(line))
		if !strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "     ") {
			t.Errorf("row not in the list column: %q", line)
		}
	}
	for _, w := range starts[1:] {
		if w != starts[0] {
			t.Errorf("rows are %v columns wide, want them equal:\n%s", starts, got)
			break
		}
	}
}

// U2: conflict rows pad the same way.
func TestConflictRows_AlignWideNames(t *testing.T) {
	names := []string{"富士山.jpeg", "notes.txt", "회의록.docx"}
	conflicts := make([]transfer.Conflict, len(names))
	for i := range conflicts {
		conflicts[i] = transfer.Conflict{Kind: "file vs folder"}
	}
	var widths []int
	for _, row := range conflictRows(conflicts, names) {
		name, _, ok := strings.Cut(row, uxlog.Gap+"file vs folder")
		if !ok {
			t.Fatalf("row without its label column: %q", row)
		}
		widths = append(widths, uxlog.Width(name))
	}
	for _, w := range widths {
		if w != widths[0] || w != uxlog.Width("富士山.jpeg") {
			t.Errorf("name columns are %v columns wide, want all %d", widths, uxlog.Width("富士山.jpeg"))
			break
		}
	}
}
