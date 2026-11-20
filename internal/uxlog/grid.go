package uxlog

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// The grid. Every line fsend prints has a one-column gutter: the status
// glyph sits in column 0 (or nothing, for a neutral line), the text starts
// in column 2, list rows start in column 4, and the share command — alone
// on its line, so a triple-click copies a runnable command — in column 6.
//
// The functions in this file are the only way a line reaches the screen:
// they own the indent, so a caller picks a role (Status, Line, Note, Item,
// Command, Question) and never writes leading spaces of its own.
// cmd/fsend's TestGridIsTheOnlyWayToPrint enforces that.
const (
	colText    = "  "     // column 2: the sentence, under the glyph's gutter
	colItem    = "    "   // column 4: a row of a list
	colCommand = "      " // column 6: the command to copy
)

// Gap separates two columns of one row: a file's name from its size, a
// conflict's size change from its age.
const Gap = "   "

// Printer prints lines on the grid to one writer. The package-level
// functions (Status, Line, …) are the Printer for stderr; On gives one for
// another writer — stdout for a query's answer, a buffer in tests.
type Printer struct {
	w io.Writer // nil: stderr, looked up at each call
}

// On returns the Printer for w. Stderr's Printer cooperates with whatever
// is drawing there: a line lands above a live progress bar, and wipes a
// live spinner's frame first.
func On(w io.Writer) Printer {
	if f, ok := w.(*os.File); ok && f == os.Stderr {
		return stderrPrinter
	}
	return Printer{w: w}
}

var stderrPrinter = Printer{}

// println writes one finished line.
func (p Printer) println(line string) {
	if p.w == nil {
		printLine(line)
		return
	}
	_, _ = fmt.Fprintln(p.w, line)
}

// Status prints a line with glyph in column 0 (Check, Cross, Warn, Ask,
// an arrow, Info's blank) and text from column 2. A text of several lines
// brings its own rows (Row, Detail, ItemRow).
//
// On a terminal each row of the block that would run past the right edge
// breaks between words, the continuation in the row's own column (2 for
// the headline), so the terminal never wraps a line into column 0.
func (p Printer) Status(glyph, text string) {
	if p.w == nil {
		text = wrapBlock(text, terminalWidth(0))
	}
	p.println(glyph + " " + text)
}

// wrapBlock wraps every row of a status block to width columns: the first
// row from column 2, every other row from its own leading spaces. Width 0
// (not a terminal) leaves the block as it is.
func wrapBlock(text string, width int) string {
	if width <= 0 {
		return text
	}
	rows := strings.Split(text, "\n")
	for i, row := range rows {
		indent, body := len(colText), row
		if i > 0 {
			body = strings.TrimLeft(row, " ")
			indent = len(row) - len(body)
		}
		if indent+visibleWidth(body) < width {
			continue
		}
		pad := strings.Repeat(" ", indent)
		lead := pad
		if i == 0 {
			lead = "" // the glyph and its space fill columns 0 and 1
		}
		rows[i] = lead + strings.Join(wrapWordsAt(body, indent, width), "\n"+pad)
	}
	return strings.Join(rows, "\n")
}

// Line prints a neutral line: no glyph, text from column 2.
// On a terminal it wraps like Status.
func (p Printer) Line(text string) {
	if p.w == nil {
		text = wrapBlock(text, terminalWidth(0))
	}
	p.println(Row(text))
}

// LineCommand prints a label and the command it introduces ("Resume
// with: fsend …") on one line when that fits the terminal, else the label
// and then the command on a row of its own, so the command is never broken.
func LineCommand(label, cmd string) {
	line := label + " " + Code(cmd)
	if w := terminalWidth(0); w > 0 && len(colText)+visibleWidth(line) >= w {
		Line(label)
		stderrPrinter.Command(Code(cmd))
		return
	}
	Line(line)
}

// Note prints faint detail in column 2, under the line it belongs to.
// On a terminal it wraps like Status.
func (p Printer) Note(text string) {
	if p.w == nil {
		text = wrapBlock(text, terminalWidth(0))
	}
	p.println(Detail(text))
}

// NoteWrapped is Note for a sentence that may be longer than the terminal
// is wide: it breaks between words, every row a Note from column 2.
func (p Printer) NoteWrapped(text string) {
	for _, row := range wrapWords(text, len(colText)) {
		p.Note(row)
	}
}

// Item prints one row of a list, from column 4.
func (p Printer) Item(text string) { p.println(ItemRow(text)) }

// Command prints the command to run on its own line, from column 6.
func (p Printer) Command(text string) { p.println(CommandRow(text)) }

// Blank prints an empty line.
func (p Printer) Blank() { p.println("") }

// Status prints a glyph-led line on stderr. See Printer.Status.
func Status(glyph, text string) { stderrPrinter.Status(glyph, text) }

// Line prints a neutral line on stderr. See Printer.Line.
func Line(text string) { stderrPrinter.Line(text) }

// Note prints a faint detail line on stderr. See Printer.Note.
func Note(text string) { stderrPrinter.Note(text) }

// NoteWrapped prints a faint, word-wrapped detail line on stderr. See
// Printer.NoteWrapped.
func NoteWrapped(text string) { stderrPrinter.NoteWrapped(text) }

// Item prints a list row on stderr. See Printer.Item.
func Item(text string) { stderrPrinter.Item(text) }

// Blank prints an empty line on stderr.
func Blank() { stderrPrinter.Blank() }

// Debug prints one --debug line on stderr: neutral, in column 2, tagged so
// it can be told from the output it sits in.
func Debug(text string) { stderrPrinter.Line("DEBUG: " + text) }

// StatusAfterInterrupt is Status for the line that answers a Ctrl-C. The
// terminal has just echoed "^C", leaving the cursor two columns in, so the
// line first returns to column 0. Off a terminal nothing was echoed and no
// carriage return is written.
func StatusAfterInterrupt(glyph, text string) {
	line := glyph + " " + wrapBlock(text, terminalWidth(0))
	if IsTTY(os.Stderr) {
		line = "\r" + line
	}
	printLine(line)
}

// Row is Line's string form, for a caller assembling a block of several
// lines (an error with its advice): text from column 2.
func Row(text string) string { return colText + text }

// Detail is Note's string form: faint text from column 2.
func Detail(text string) string { return colText + Muted(text) }

// ItemRow is Item's string form: text from column 4.
func ItemRow(text string) string { return colItem + text }

// CommandRow is Command's string form: text from column 6.
func CommandRow(text string) string { return colCommand + text }

// Question prints a prompt that offers choices and leaves the cursor
// after it: the ? glyph in column 0, the question from column 2, then the
// options ("[Y/n]"), as in
//
//	? Save to ./Downloads/  [Y/n] _
//
// Every line that waits for an answer starts with ?, so a glance down the
// gutter finds it.
//
// When the line won't fit the terminal, the question wraps and the options
// go on rows of their own from column 2, broken only between choices, with
// the cursor after the last.
func Question(text, options string) {
	line := QuestionLine(text, options)
	if w := terminalWidth(0); w > 0 && visibleWidth(line) >= w {
		line = Ask() + " " + wrapBlock(text, w) + "\n" + colText + strings.Join(packChoices(options, w-1-len(colText)), "\n"+colText) + " "
	}
	prompt(line)
}

// packChoices breaks an options string ("[K]eep both · [r]eplace · [n]o")
// into rows no wider than avail, only between choices.
func packChoices(options string, avail int) []string {
	const sep = " · "
	var rows []string
	row := ""
	for _, c := range strings.Split(options, sep) {
		switch {
		case row == "":
			row = c
		case visibleWidth(row+sep+c) > avail:
			rows = append(rows, row)
			row = c
		default:
			row += sep + c
		}
	}
	return append(rows, row)
}

// QuestionLine is Question's string form.
func QuestionLine(text, options string) string {
	return Ask() + " " + text + "  " + options + " "
}

// Entry prints a prompt that takes typed text — a code, a password — and
// leaves the cursor after it: the ? glyph in column 0 and the label in the
// question's colour, as in
//
//	? Enter the password the sender shared: _
//
// A label too wide for the terminal wraps, the cursor after its last row.
func Entry(label string) {
	line := EntryLine(label)
	if w := terminalWidth(0); w > 0 && visibleWidth(line) >= w {
		line = Ask() + " " + wrapBlock(Prompt(strings.TrimRight(label, " ")), w) + " "
	}
	prompt(line)
}

// EntryLine is Entry's string form.
func EntryLine(label string) string {
	return Ask() + " " + Prompt(strings.TrimRight(label, " ")+" ")
}

// prompt writes a prompt line with no newline, wiping a live spinner's
// frame first so the question starts in column 0.
func prompt(line string) {
	ttyMu.Lock()
	defer ttyMu.Unlock()
	if activeSpinner() != nil {
		_, _ = fmt.Fprint(os.Stderr, "\r\x1b[2K")
	}
	_, _ = fmt.Fprint(os.Stderr, line)
}

// Control writes a terminal control sequence that draws nothing — OSC 52,
// which sets the clipboard — to stderr. The caller checks CanEscape first.
func Control(seq string) {
	ttyMu.Lock()
	defer ttyMu.Unlock()
	_, _ = fmt.Fprint(os.Stderr, seq)
}

// Shown is how many rows of an n-row list to print when at most limit fit
// before the rest collapse into an "… and N more" line. That line costs a
// row itself, so it never stands for exactly one: a list one over the
// limit is printed whole.
func Shown(n, limit int) int {
	if n <= limit+1 {
		return n
	}
	return limit
}
