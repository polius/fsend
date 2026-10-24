package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// A usage error leads with its specific reason; the generic "Invalid
// usage." / "Run fsend --help" pair would only bury it.
func TestRenderError_UsageHeadline(t *testing.T) {
	err := fmt.Errorf("%w: nothing to send — the folder is empty", fserrors.ErrUsage)
	var code int
	got := captureStderr(t, func() { code = renderError(err, false) })
	if !strings.Contains(got, "[E024] Nothing to send — the folder is empty") {
		t.Errorf("specific reason should be the headline:\n%s", got)
	}
	for _, unwanted := range []string{"Invalid usage", "fsend --help"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("generic %q should be dropped when a reason exists:\n%s", unwanted, got)
		}
	}
	if code != 24 {
		t.Errorf("exit = %d, want 24", code)
	}
	// Without a reason, the generic pair stays.
	got = captureStderr(t, func() { renderError(fserrors.ErrUsage, false) })
	if !strings.Contains(got, "Invalid usage.") || !strings.Contains(got, "fsend --help") {
		t.Errorf("bare usage error lost its generic text:\n%s", got)
	}
}

// The user's own "no" at a prompt exits 0; the sender whose receiver
// declined still sees a failure.
func TestRenderError_DeliberateChoiceExitsZero(t *testing.T) {
	t.Cleanup(func() { errorRole = "" })
	errorRole = "receiver"
	if code := renderErrorQuiet(t, fserrors.ErrReceiverDeclined); code != 0 {
		t.Errorf("receiver's own decline exit = %d, want 0", code)
	}
	if code := renderErrorQuiet(t, errKeptByChoice); code != 0 {
		t.Errorf("kept-by-choice exit = %d, want 0", code)
	}
	if code := renderErrorQuiet(t, fserrors.ErrTargetExists); code != 13 {
		t.Errorf("unattended kept files exit = %d, want 13", code)
	}
	errorRole = "sender"
	if code := renderErrorQuiet(t, fserrors.ErrReceiverDeclined); code != 6 {
		t.Errorf("sender seeing a decline exit = %d, want 6", code)
	}
}

func renderErrorQuiet(t *testing.T, err error) (code int) {
	t.Helper()
	captureStderr(t, func() { code = renderError(err, false) })
	return code
}

// An interrupted receive names the exact command that resumes it.
func TestRenderError_ReceiverResumeCommand(t *testing.T) {
	t.Cleanup(func() { errorRole, receiveResume = "", "" })
	errorRole = "receiver"
	receiveResume = resumeCommand(&flags{outDir: "dl"}, "abc-defg-jkm")
	got := captureStderr(t, func() { renderError(fserrors.ErrTransientFailure, false) })
	if !strings.Contains(got, "Resume with: fsend abc-defg-jkm --out dl") {
		t.Errorf("missing the resume command:\n%s", got)
	}
}

// Typos of the words other CLIs use as subcommands get a "did you mean".
func TestSubcommandHint(t *testing.T) {
	for in, want := range map[string]string{
		"recieve":    `no "receive" subcommand`,
		"send":       `no "send" subcommand`,
		"verison":    "fsend --version",
		"updte":      "fsend --update",
		"sever":      "Did you mean: fsend server",
		"doctr":      "Did you mean: fsend doctor",
		"report.pdf": "",
		"sender":     "",
	} {
		got := subcommandHint(in)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("subcommandHint(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisplayPath_RelativeToWorkingDir(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	wd, _ := os.Getwd() // the resolved form (macOS /private/var/...)
	sep := string(filepath.Separator)
	if got := displayPath(wd); got != "." {
		t.Errorf("cwd = %q, want .", got)
	}
	if got := displayPath(filepath.Join(wd, "dl", "a.txt")); got != "."+sep+filepath.Join("dl", "a.txt") {
		t.Errorf("under cwd = %q", got)
	}
	if got := saveTargetLabel(wd); got != "./" {
		t.Errorf("save target for cwd = %q, want ./", got)
	}
}

// A single saved file is named by its path; a folder by name and place.
func TestReceiverHeadline_NamesTheSavedFile(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	dir := t.TempDir()
	t.Chdir(dir)
	wd, _ := os.Getwd()
	ui := &receiverUI{outDir: filepath.Join(wd, "dl")}
	h := &wire.SenderHello{Mode: wire.ModeFiles, DisplayName: "report.pdf"}
	got := ui.headline(h, []string{filepath.Join(wd, "dl", "report.pdf")})
	if want := "Saved ." + string(filepath.Separator) + filepath.Join("dl", "report.pdf"); !strings.Contains(got, want) {
		t.Errorf("single file headline = %q, want %q", got, want)
	}
	h = &wire.SenderHello{Mode: wire.ModeFiles, DisplayName: "proj/"}
	got = ui.headline(h, []string{filepath.Join(wd, "dl", "proj", "a"), filepath.Join(wd, "dl", "proj", "b")})
	if !strings.Contains(got, "Saved proj/ to ."+string(filepath.Separator)+"dl/") {
		t.Errorf("folder headline = %q", got)
	}
}

// A dropped receiver is named, with how far it got and the code to rerun.
func TestDropNotice(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	t.Cleanup(func() { sendSession = sessionState{} })
	sendSession = sessionState{}
	sendSession.setPeer("bobs-laptop")
	sendSession.setPct(52)
	got := dropNotice(true, "abc-defg-jkm")
	if want := "bobs-laptop stopped at 52% — the code still works: fsend abc-defg-jkm"; got != want {
		t.Errorf("deliberate = %q, want %q", got, want)
	}
	sendSession = sessionState{}
	if got := dropNotice(false, "abc-defg-jkm"); !strings.HasPrefix(got, "Lost contact with the receiver —") {
		t.Errorf("unnamed lost contact = %q", got)
	}
}

func TestSendSubject(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	for _, c := range []struct {
		plan sendPlan
		want string
	}{
		{sendPlan{mode: wire.ModeFiles, label: "report.pdf", totalFiles: 1}, "report.pdf"},
		{sendPlan{mode: wire.ModeFiles, totalFiles: 3}, "3 files"},
		{sendPlan{mode: wire.ModeStream, isText: true}, "text"},
		{sendPlan{mode: wire.ModeStream}, "stdin"},
	} {
		if got := sendSubject(&c.plan); got != c.want {
			t.Errorf("sendSubject(%+v) = %q, want %q", c.plan, got, c.want)
		}
	}
}

func TestWantsBar(t *testing.T) {
	if wantsBar(999_999, false) {
		t.Error("sub-MB transfer should not get a bar")
	}
	if !wantsBar(1_000_000, false) || !wantsBar(0, true) {
		t.Error("MB-scale transfers and unknown-size streams get a bar")
	}
}

// --as names a piped stream on the receiver's disk; it is no path.
func TestCollectPlan_As(t *testing.T) {
	plan, err := collectPlan(&flags{as: "db.sql"}, []string{"-"})
	if err != nil || plan.displayName != "db.sql" {
		t.Fatalf("collectPlan(--as db.sql, -) = %+v, %v", plan, err)
	}
	for _, bad := range []string{"../x", "a/b", ".."} {
		if _, err := collectPlan(&flags{as: bad}, []string{"-"}); err == nil {
			t.Errorf("--as %q accepted", bad)
		}
	}
}
