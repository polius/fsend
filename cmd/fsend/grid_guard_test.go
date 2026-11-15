package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The grid — glyph in column 0, text from column 2, list rows from column
// 4 — holds because the printers in internal/uxlog (grid.go) own the
// indent and are the only way a line reaches the screen. This test keeps
// it that way: it parses the package's non-test source and fails on
//
//   - a write that bypasses uxlog: any fmt.Print*/Fprint* call, an
//     os.Stderr method that writes, os.Stderr handed to io/bufio, a
//     cobra command's own printers (cmd.Println, cmd.PrintErrln), the
//     print/println builtins;
//   - a hand-written indent: a string literal that begins with two spaces,
//     or with one where it is the text given to a uxlog printer, or that
//     has an indented second line; strings.Repeat of spaces;
//   - a hand-written glyph: a string literal in a status line's gutter,
//     which takes one of uxlog's markers (Check, Cross, Warn, Ask, Info,
//     Retry, SendArrow, RecvArrow).
//
// The few functions that write a machine payload or a stdout page (JSON,
// the bare code, --help) are listed below with the reason. To print a new
// line, pick a role: uxlog.Status, Line, Note, Item, Question, Entry.

// gridExemptFiles are not scanned yet.
var gridExemptFiles = map[string]string{
	// doctor's report has a label column of its own; its conversion to the
	// grid printers is tracked separately. Remove these once it lands.
	"doctor.go":                 "doctor's report is converted separately",
	"doctor_firewall_darwin.go": "doctor's report is converted separately",
	"doctor_firewall_other.go":  "doctor's report is converted separately",
}

// gridRawWriters are the top-level declarations allowed to call fmt's
// printers directly: none of them writes a line of the stderr screen.
var gridRawWriters = map[string]string{
	"jsonout.go:jsonEvent":           "NDJSON events on stdout",
	"send.go:printBareCode":          "the bare share code on stdout, for scripts",
	"receiverui.go:printTextPayload": "the newline that ends a received text payload on stdout",
	"root.go:helpCmd":                "the help page on stdout",
	"root.go:startHere":              "the start-here page on stdout",
	"server.go:runServer":            "the server's configuration dump on stdout, ahead of its logs",
	"server.go:formatServerConfig":   "builds that dump in a strings.Builder",
}

// gridRawText are the top-level declarations whose string literals may
// carry their own layout: pages and tables that go to stdout whole.
var gridRawText = map[string]string{
	"root.go:rootLong":               "the root command's long description (man page)",
	"root.go:helpTemplate":           "the --help page",
	"root.go:advancedHelpTemplate":   "the `help advanced` page",
	"root.go:completionHelpTemplate": "the `fsend completion --help` page",
	"root.go:decorateHelpFlags":      "reads the help pages' own layout to colour their flags",
	"root.go:startHere":              "the start-here page",
	"server.go:serverHelpTemplate":   "the `fsend server --help` page",
	"server.go:formatServerConfig":   "the server's configuration table",
}

// gridPrinters are the uxlog functions that put text on the grid, with the
// index of the argument that is the text.
var gridPrinters = map[string]int{
	"Status": 1, "StatusAfterInterrupt": 1,
	"Line": 0, "Note": 0, "NoteWrapped": 0, "Item": 0, "Command": 0, "Debug": 0,
	"Row": 0, "Detail": 0, "ItemRow": 0, "CommandRow": 0,
	"Question": 0, "Entry": 0, "QuestionLine": 0, "EntryLine": 0,
}

func TestGridIsTheOnlyWayToPrint(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	scanned := 0
	usedWriters, usedText := map[string]bool{}, map[string]bool{}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		if _, skip := gridExemptFiles[name]; skip {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		scanned++
		for _, decl := range f.Decls {
			for _, d := range declNames(decl) {
				key := name + ":" + d.name
				_, rawWriter := gridRawWriters[key]
				_, rawText := gridRawText[key]
				for _, v := range gridViolations(d.node, rawWriter, rawText, func() { usedWriters[key] = true }, func() { usedText[key] = true }) {
					pos := fset.Position(v.pos)
					violations = append(violations, fmt.Sprintf("%s:%d: %s", pos.Filename, pos.Line, v.msg))
				}
			}
		}
	}
	if scanned < 20 {
		t.Fatalf("scanned only %d files: run from cmd/fsend", scanned)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
	// An allowance nothing uses any more is a hole waiting to be used.
	for key := range gridRawWriters {
		if !usedWriters[key] {
			t.Errorf("gridRawWriters lists %s, which no longer prints directly: remove the entry", key)
		}
	}
	for key := range gridRawText {
		if !usedText[key] {
			t.Errorf("gridRawText lists %s, which no longer lays out its own text: remove the entry", key)
		}
	}
	for name := range gridExemptFiles {
		if _, err := os.Stat(name); err != nil {
			t.Errorf("gridExemptFiles lists %s, which is gone: remove the entry", name)
		}
	}
}

// The guard itself: each rule fires on the code it is meant to stop, and
// stays quiet on what the package legitimately writes.
func TestGridGuardRules(t *testing.T) {
	for _, c := range []struct {
		name, body string
		want       string // "" = clean
	}{
		{"stderr via fmt", `fmt.Fprintln(os.Stderr, "x")`, "fmt.Fprintln"},
		{"stdout via fmt", `fmt.Println("x")`, "fmt.Println"},
		{"any writer via fmt", `fmt.Fprintf(w, "%s\n", x)`, "fmt.Fprintf"},
		{"stderr method", `os.Stderr.WriteString("x")`, "os.Stderr.WriteString"},
		{"stderr via io", `io.WriteString(os.Stderr, "x")`, "os.Stderr"},
		{"cobra's stderr printer", `cmd.PrintErrln("x")`, "cmd.PrintErrln"},
		{"cobra's stdout printer", `cmd.Printf("%s\n", x)`, "cmd.Printf"},
		{"cobra's printer on a call's result", `cmd.Root().PrintErrf("%s\n", x)`, "PrintErrf"},
		{"the println builtin", `println("x")`, "println"},
		{"two-space indent", `uxlog.Line("  nested")`, "indent"},
		{"indent kept in a variable", `pad := "    "; _ = pad`, "indent"},
		{"indented second line", `x := "first\n  second"; _ = x`, "indent"},
		{"one stray space", `uxlog.Line(" text")`, "leading space"},
		{"stray space in a format", `uxlog.Status(g, fmt.Sprintf(" %s", x))`, "leading space"},
		{"stray space in a concatenation", `uxlog.Item(" - " + x)`, "leading space"},
		{"spaces repeated", `x := strings.Repeat(" ", n); _ = x`, "strings.Repeat"},
		{"a seventh glyph", `uxlog.Status("›", "Checking the latest release")`, "glyph"},
		{"a blank gutter by hand", `uxlog.Status(" ", "Checking")`, "glyph"},
		{"a glyph chosen earlier", `uxlog.Status(glyph, headline)`, ""},
		{"a status line", `uxlog.Status(uxlog.Check(), "Sent "+name)`, ""},
		{"a formatted line", `uxlog.Line(fmt.Sprintf("%q matches a code", arg))`, ""},
		{"a continuation", `s += " to " + peer`, ""},
		{"a separator", `x := strings.Join(args, " "); _ = x`, ""},
		{"inner alignment", `uxlog.Line("Set a new server:       fsend --connect")`, ""},
		{"an error", `return fmt.Errorf("%w: no code entered", err)`, ""},
		{"stderr inspected", `_ = term.IsTerminal(int(os.Stderr.Fd()))`, ""},
		{"stderr handed to a printer", `uxlog.On(os.Stderr).Line("x")`, ""},
	} {
		src := "package p\nfunc f() {\n" + c.body + "\n}\n"
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		got := gridViolations(f.Decls[0], false, false, func() {}, func() {})
		switch {
		case c.want == "" && len(got) > 0:
			t.Errorf("%s: flagged %q", c.name, got[0].msg)
		case c.want != "" && len(got) == 0:
			t.Errorf("%s: not flagged", c.name)
		case c.want != "" && !strings.Contains(got[0].msg, c.want):
			t.Errorf("%s: flagged as %q, want a mention of %q", c.name, got[0].msg, c.want)
		}
	}
}

type namedDecl struct {
	name string
	node ast.Node
}

// declNames splits a top-level declaration into its named parts: a
// function, or each const/var of a block.
func declNames(decl ast.Decl) []namedDecl {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		return []namedDecl{{d.Name.Name, d}}
	case *ast.GenDecl:
		var out []namedDecl
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && len(vs.Names) > 0 {
				out = append(out, namedDecl{vs.Names[0].Name, vs})
			}
		}
		return out
	}
	return nil
}

type gridViolation struct {
	pos token.Pos
	msg string
}

// gridViolations applies the rules to one declaration. rawWriter and
// rawText lift the matching rule; usedWriter/usedText report that the
// allowance was needed.
func gridViolations(node ast.Node, rawWriter, rawText bool, usedWriter, usedText func()) []gridViolation {
	var out []gridViolation
	add := func(pos token.Pos, format string, args ...any) {
		out = append(out, gridViolation{pos, fmt.Sprintf(format, args...)})
	}
	ast.Inspect(node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if id, ok := n.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
				add(n.Pos(), "the %s builtin writes to stderr past the grid: use a uxlog printer", id.Name)
				return true
			}
			pkg, fn := callee(n)
			switch {
			case pkg == "fmt" && (strings.HasPrefix(fn, "Print") || strings.HasPrefix(fn, "Fprint")):
				if rawWriter {
					usedWriter()
				} else {
					add(n.Pos(), "fmt.%s writes past the grid: use a uxlog printer (Status, Line, Note, Item, Question, Entry)", fn)
				}
			case pkg == "os.Stderr" && fn != "Fd" && fn != "Stat":
				add(n.Pos(), "os.Stderr.%s writes past the grid: use a uxlog printer", fn)
			case pkg != "uxlog" && isPrintMethod(fn):
				// A cobra command's printers (cmd.Println, cmd.PrintErrf):
				// a second way to stdout and stderr.
				if pkg == "" {
					pkg = "(a command)"
				}
				add(n.Pos(), "%s.%s writes past the grid: use a uxlog printer", pkg, fn)
			case pkg == "io" || pkg == "bufio":
				for _, a := range n.Args {
					if isStderr(a) {
						add(n.Pos(), "os.Stderr handed to %s.%s: use a uxlog printer", pkg, fn)
					}
				}
			case pkg == "strings" && fn == "Repeat" && len(n.Args) > 0:
				if s, ok := stringLit(n.Args[0]); ok && s != "" && strings.TrimSpace(s) == "" {
					add(n.Pos(), "strings.Repeat of spaces is a hand-written indent or pad: use uxlog.PadRight, or a uxlog printer for the indent")
				}
			case pkg == "uxlog":
				if strings.HasPrefix(fn, "Status") && len(n.Args) > 0 {
					if _, lit := leadingLit(n.Args[0]); lit {
						add(n.Args[0].Pos(), "hand-written glyph in uxlog.%s: the gutter takes one of uxlog's markers (Check, Cross, Warn, Ask, Info, Retry, SendArrow, RecvArrow)", fn)
					}
				}
				if i, ok := gridPrinters[fn]; ok && i < len(n.Args) {
					if s, ok := leadingLit(n.Args[i]); ok && strings.HasPrefix(s, " ") && !strings.HasPrefix(s, "  ") {
						add(n.Args[i].Pos(), "leading space in the text given to uxlog.%s: the printer owns the indent", fn)
					}
				}
			}
		case *ast.BasicLit:
			s, ok := stringLit(n)
			if !ok || !indented(s) {
				return true
			}
			if rawText {
				usedText()
			} else {
				add(n.Pos(), "hand-written indent in %s: the uxlog printers own the indent (Line, Item, Row, ItemRow; uxlog.Gap between columns)", strconv.Quote(clip(s)))
			}
		}
		return true
	})
	return out
}

// indented reports a literal that carries an indent of its own: it starts
// with two spaces or a tab, or a later line of it starts with a space or
// a tab.
func indented(s string) bool {
	if strings.HasPrefix(s, "  ") || strings.HasPrefix(s, "\t") {
		return true
	}
	for _, line := range strings.Split(s, "\n")[1:] {
		if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			return true
		}
	}
	return false
}

// callee names a call as (package or receiver, function): ("fmt",
// "Fprintln"), ("os.Stderr", "WriteString"), ("uxlog", "Line"). A method
// on a printer value (uxlog.On(w).Line, out.Item) counts as uxlog's.
func callee(c *ast.CallExpr) (pkg, fn string) {
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	fn = sel.Sel.Name
	switch x := sel.X.(type) {
	case *ast.Ident:
		if _, printer := gridPrinters[fn]; printer && x.Name != "uxlog" && isPrinterName(x.Name) {
			return "uxlog", fn
		}
		return x.Name, fn
	case *ast.SelectorExpr:
		if id, ok := x.X.(*ast.Ident); ok {
			return id.Name + "." + x.Sel.Name, fn
		}
	case *ast.CallExpr:
		if p, f := callee(x); p == "uxlog" && f == "On" {
			return "uxlog", fn
		}
	}
	return "", fn
}

// isPrintMethod recognises the names of cobra's command printers (and of
// any other value that prints the way fmt does).
func isPrintMethod(fn string) bool {
	switch fn {
	case "Print", "Println", "Printf", "PrintErr", "PrintErrln", "PrintErrf":
		return true
	}
	return false
}

// isPrinterName recognises the local names this package gives a
// uxlog.Printer.
func isPrinterName(name string) bool {
	return name == "out" || name == "answer"
}

func isStderr(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == "os" && sel.Sel.Name == "Stderr"
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	return s, err == nil
}

// leadingLit returns the string literal a text expression starts with: the
// literal itself, the leftmost operand of a concatenation, or the format
// of an fmt.Sprintf.
func leadingLit(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		return stringLit(x)
	case *ast.BinaryExpr:
		return leadingLit(x.X)
	case *ast.ParenExpr:
		return leadingLit(x.X)
	case *ast.CallExpr:
		if pkg, fn := callee(x); pkg == "fmt" && fn == "Sprintf" && len(x.Args) > 0 {
			return leadingLit(x.Args[0])
		}
	}
	return "", false
}

func clip(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > 40 {
		s = s[:40] + "…"
	}
	return s
}
