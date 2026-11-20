package uxlog

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
)

// captureStderr swaps os.Stderr for a pipe while fn runs. A pipe is not a
// terminal, so every marker takes its ASCII form and no colour is written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stderr = old
	return <-done
}

// Each printer puts its text in the column its role names, and nothing
// else decides an indent.
func TestPrinterColumns(t *testing.T) {
	var b bytes.Buffer
	p := On(&b)
	p.Status("[OK]", "Sent report.pdf")
	p.Line("fsend uninstalled.")
	p.Note("2.4 MB · 0.3 s")
	p.Item("assets/video1.bin")
	p.Command("fsend abc-defg-jkm")
	p.Blank()
	want := "[OK] Sent report.pdf\n" +
		"  fsend uninstalled.\n" +
		"  2.4 MB · 0.3 s\n" +
		"    assets/video1.bin\n" +
		"      fsend abc-defg-jkm\n" +
		"\n"
	if got := b.String(); got != want {
		t.Errorf("printed\n%q\nwant\n%q", got, want)
	}
	for _, c := range []struct {
		got, want string
	}{
		{Row("x"), "  x"},
		{Detail("x"), "  x"},
		{ItemRow("x"), "    x"},
		{CommandRow("x"), "      x"},
	} {
		if c.got != c.want {
			t.Errorf("row = %q, want %q", c.got, c.want)
		}
	}
}

// The stderr printers follow os.Stderr as it is when they are called, and
// write the same bytes as the Printer for any other writer.
func TestStderrPrinters(t *testing.T) {
	got := captureStderr(t, func() {
		Status(Check(), "Reverted to default server: example.org:443")
		Line("Config file: /tmp/x")
		Note("1 file · 52 B")
		NoteWrapped("One receiver · valid while this runs")
		Item("- binary:     /usr/local/bin/fsend")
		Blank()
		Debug("ICE failed: timeout")
		On(os.Stderr).Line("through On")
	})
	want := "[OK] Reverted to default server: example.org:443\n" +
		"  Config file: /tmp/x\n" +
		"  1 file · 52 B\n" +
		"  One receiver · valid while this runs\n" +
		"    - binary:     /usr/local/bin/fsend\n" +
		"\n" +
		"  DEBUG: ICE failed: timeout\n" +
		"  through On\n"
	if got != want {
		t.Errorf("stderr\n%q\nwant\n%q", got, want)
	}
}

// V7: every line that waits for an answer starts with the ? gutter mark.
// A choice prompt puts its options two spaces after the question; a text
// prompt ends in its colon. Both leave the cursor one space on, with no
// newline.
func TestPrompts(t *testing.T) {
	if got, want := QuestionLine("Continue?", "[y/N]"), "[?] Continue?  [y/N] "; got != want {
		t.Errorf("QuestionLine = %q, want %q", got, want)
	}
	if got, want := EntryLine("Code from the sender:"), "[?] Code from the sender: "; got != want {
		t.Errorf("EntryLine = %q, want %q", got, want)
	}
	// A label that already ends in a space gets no second one.
	if got, want := EntryLine("Enter the password: "), "[?] Enter the password: "; got != want {
		t.Errorf("EntryLine with a trailing space = %q, want %q", got, want)
	}
	got := captureStderr(t, func() {
		Question("Save to ./Downloads/?", "[Y/n]")
		Entry("Enter the password the sender shared:")
	})
	want := "[?] Save to ./Downloads/?  [Y/n] " + "[?] Enter the password the sender shared: "
	if got != want {
		t.Errorf("prompts = %q, want %q", got, want)
	}
}

// A prompt printed while a spinner animates starts on a cleared line.
func TestPromptClearsLiveSpinner(t *testing.T) {
	var frames bytes.Buffer
	s := &Spinner{msg: "Waiting", w: &frames, tty: true}
	setActiveSpinner(s)
	defer clearActiveSpinner(s)
	got := captureStderr(t, func() { Question("Save?", "[Y/n]") })
	if !strings.HasPrefix(got, "\r\x1b[2K") || !strings.HasSuffix(got, "Save?  [Y/n] ") {
		t.Errorf("prompt over a live spinner = %q", got)
	}
}

// Off a terminal nothing echoed "^C", so the line that answers it carries
// no carriage return: logs stay free of control characters.
func TestStatusAfterInterruptOffTerminal(t *testing.T) {
	got := captureStderr(t, func() { StatusAfterInterrupt(Info(), "Cancelling…") })
	if want := "[i] Cancelling…\n"; got != want {
		t.Errorf("off a terminal = %q, want %q", got, want)
	}
}

// U8: a list never hides exactly one row — the "… and 1 more" line would
// take the row it saves.
func TestShown(t *testing.T) {
	for _, c := range []struct{ n, limit, want int }{
		{0, 10, 0},
		{1, 10, 1},
		{9, 10, 9},
		{10, 10, 10},
		{11, 10, 11}, // one over: printed whole
		{12, 10, 10}, // "… and 2 more"
		{500, 10, 10},
		{6, 5, 6},
		{7, 5, 5},
		{1, 0, 1},
		{2, 0, 0},
	} {
		if got := Shown(c.n, c.limit); got != c.want {
			t.Errorf("Shown(%d, %d) = %d, want %d", c.n, c.limit, got, c.want)
		}
	}
	for limit := 0; limit <= 12; limit++ {
		for n := 0; n <= 40; n++ {
			if hidden := n - Shown(n, limit); hidden == 1 {
				t.Errorf("Shown(%d, %d) hides exactly one row", n, limit)
			}
		}
	}
}

// Control writes its sequence verbatim, with no newline.
func TestControl(t *testing.T) {
	if got := captureStderr(t, func() { Control("\x1b]52;c;eA==\a") }); got != "\x1b]52;c;eA==\a" {
		t.Errorf("Control wrote %q", got)
	}
}

// A status block wider than the terminal breaks between words, each
// continuation in its row's own column; escapes take no width; off a
// terminal (width 0) nothing changes.
func TestWrapBlock(t *testing.T) {
	head := "Wrong password from laptop — still waiting; they can rerun fsend abc-defg-jkm with the right password."
	got := wrapBlock(head+"\n"+Detail("To resume, rerun on that device: fsend abc-defg-jkm --out ./downloads/here"), 40)
	for i, row := range strings.Split(got, "\n") {
		plain := escapes.ReplaceAllString(row, "")
		if w := 2*boolToInt(i == 0) + Width(plain); w >= 40 {
			t.Errorf("row %d is %d columns: %q", i, w, plain)
		}
		if i > 0 && !strings.HasPrefix(plain, "  ") {
			t.Errorf("row %d must continue in column 2: %q", i, plain)
		}
	}
	if !strings.Contains(escapes.ReplaceAllString(got, ""), "abc-defg-jkm") {
		t.Errorf("a word must never be split: %q", got)
	}
	if wrapBlock(head, 0) != head {
		t.Error("width 0 must leave the block alone")
	}
	if short := "Sent · 1 file"; wrapBlock(short, 40) != short {
		t.Error("a row that fits must be left alone")
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
