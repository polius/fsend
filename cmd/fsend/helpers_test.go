package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/polius/fsend/internal/config"
	"github.com/polius/fsend/internal/connpath"
	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/transfer"
	"github.com/polius/fsend/internal/wire"
)

func mustLANInfo() connpath.Info { return connpath.FromLAN() }

// ---------------------------------------------------------------------------
// internet.go pure helpers
// ---------------------------------------------------------------------------

func TestIsLocalAddr(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"localhost:8080", true},
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"fsend.alzina.dev:443", false},
		{"10.0.0.1:443", false},
		{"", true}, // empty host short-circuits to "local"
	} {
		if got := isLocalAddr(tc.in); got != tc.want {
			t.Errorf("isLocalAddr(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestShortErr_TruncatesAndStripsNewlines(t *testing.T) {
	// shortErr caps to 60 runes (59 + "…"), so the byte length grows by
	// the UTF-8 width of the ellipsis. Assert in runes, which is what the
	// caller cares about for layout.
	long := strings.Repeat("a", 100)
	got := shortErr(errors.New(long))
	if n := len([]rune(got)); n > 60 {
		t.Errorf("runes = %d, want ≤ 60", n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix on truncated err, got %q", got)
	}

	multi := errors.New("first line\nsecond line that should be dropped")
	if got := shortErr(multi); strings.Contains(got, "\n") {
		t.Errorf("expected newline stripped, got %q", got)
	}
}

func TestHostnameOrDefault(t *testing.T) {
	if got := hostnameOrDefault("override"); got != "override" {
		t.Errorf("override ignored: %q", got)
	}
	// Empty falls back to os.Hostname(); we don't assert the exact value
	// but it should be non-empty on a sane host.
	if got := hostnameOrDefault(""); got == "" {
		t.Errorf("empty fallback returned empty (os.Hostname misconfigured?)")
	}
}

// ---------------------------------------------------------------------------
// receive.go presentation helpers
// ---------------------------------------------------------------------------

func TestSanitizeRemote(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain ASCII", "alice-mbp", "alice-mbp"},
		{"empty → placeholder", "", "peer"},
		{"only control → placeholder", "\x00\x01\x02", "peer"},
		{"strips bidi override", "abc\u202edef", "abcdef"},
		{"strips zero-width", "ab\u200bc\u200dd", "abcd"},
		{"caps at 64 runes", strings.Repeat("x", 200), strings.Repeat("x", 64)},
		{"tabs and DEL dropped", "ab\tc\x7Fd", "abcd"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeRemote(tc.in); got != tc.want {
				t.Errorf("sanitizeRemote(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsBidi(t *testing.T) {
	bidi := []rune{0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x2066, 0x2067, 0x2068, 0x2069, 0x200E, 0x200F}
	for _, r := range bidi {
		if !isBidi(r) {
			t.Errorf("isBidi(%U) = false, want true", r)
		}
	}
	safe := []rune{'a', '0', ' ', 0x4E2D /* CJK */, 0x1F600 /* emoji */}
	for _, r := range safe {
		if isBidi(r) {
			t.Errorf("isBidi(%U) = true, want false", r)
		}
	}
}

func TestSaveTargetLabel(t *testing.T) {
	if got := saveTargetLabel("/tmp/dst"); got != "/tmp/dst/" {
		t.Errorf("explicit out = %q", got)
	}
}

func TestShortRand(t *testing.T) {
	a, b := shortRand(), shortRand()
	if len(a) != 8 || len(b) != 8 {
		t.Errorf("shortRand length: %d, %d", len(a), len(b))
	}
	if a == b {
		t.Errorf("shortRand returned identical values twice: %q", a)
	}
	for _, c := range a {
		digit := c >= '0' && c <= '9'
		letter := c >= 'a' && c <= 'z'
		if !digit && !letter {
			t.Errorf("shortRand emitted out-of-alphabet rune %q in %q", c, a)
		}
	}
}

func TestCollectPlan_Text(t *testing.T) {
	plan, err := collectPlan(&flags{textArg: "hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if plan.mode != wire.ModeStream || !plan.isText || plan.totalBytes != 5 {
		t.Errorf("plan = %+v", plan)
	}
	got, err := io.ReadAll(plan.stream)
	if err != nil || string(got) != "hello" {
		t.Errorf("stream content = %q err=%v", got, err)
	}
	if !strings.HasPrefix(plan.displayName, "fsend-text-") {
		t.Errorf("name = %q", plan.displayName)
	}
}

func TestCollectPlan_Stdin(t *testing.T) {
	plan, err := collectPlan(&flags{}, []string{"-"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.mode != wire.ModeStream || plan.isText {
		t.Errorf("expected non-text stream mode, got %+v", plan)
	}
	if plan.stream != os.Stdin {
		t.Error("expected stream backed by os.Stdin")
	}
}

// containsDirectory: tested through the real filesystem.
func TestContainsDirectory(t *testing.T) {
	dir := t.TempDir()
	filePath := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(filePath, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}

	if hasDir, err := containsDirectory([]string{filePath}); err != nil || hasDir {
		t.Errorf("files-only: hasDir=%v err=%v", hasDir, err)
	}
	if hasDir, err := containsDirectory([]string{filePath, dir}); err != nil || !hasDir {
		t.Errorf("mixed: hasDir=%v err=%v", hasDir, err)
	}
	_, err := containsDirectory([]string{filepath.Join(dir, "does-not-exist")})
	if !errors.Is(err, fserrors.ErrSourceNotFound) {
		t.Errorf("missing path → want ErrSourceNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// connect.go pure helpers
// ---------------------------------------------------------------------------

func TestNormalizeServer(t *testing.T) {
	// Explicit port preserved; bare host gets an implicit port
	// (443 for DNS names, 80 for IP literals and "localhost").
	for _, tc := range []struct {
		in, want string
	}{
		{"fs.example.com:443", "fs.example.com:443"},
		{"fs.example.com:8443", "fs.example.com:8443"},
		{"127.0.0.1:8080", "127.0.0.1:8080"},
		{"[::1]:443", "[::1]:443"},
		{"fs.example.com", "fs.example.com:443"},
		{"relay.example.com", "relay.example.com:443"},
		{"localhost", "localhost:80"},
		{"127.0.0.1", "127.0.0.1:80"},
		{"10.0.0.1", "10.0.0.1:80"},
		{"::1", "[::1]:80"},
		{"[::1]", "[::1]:80"},
		{"  fs.example.com  ", "fs.example.com:443"},
		// URL form: scheme is stripped so the documented LAN-only
		// self-hosting form (`fsend --connect http://host:8080`) lands
		// on a usable host:port instead of `[http://host:8080]:443`.
		{"http://localhost:8080", "localhost:8080"},
		{"https://fs.example.com:443", "fs.example.com:443"},
		{"https://fs.example.com", "fs.example.com:443"},
		{"http://127.0.0.1:8080", "127.0.0.1:8080"},
		{"http://localhost:8080/", "localhost:8080"},
	} {
		got, err := normalizeServer(tc.in)
		if err != nil {
			t.Errorf("normalizeServer(%q) unexpected err: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizeServer(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "   ", ":443", "host:0", "host:99999", "host:abc", "http://", "https://"} {
		if _, err := normalizeServer(bad); err == nil {
			t.Errorf("normalizeServer(%q) accepted bad input", bad)
		}
	}
}

func TestRunConnect_PersistsServerAndPassword(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "config.json")
	config.SetPathForTesting(cfgPath)
	t.Cleanup(func() { config.SetPathForTesting("") })

	// Set host:port (no pw).
	f := &flags{connectArgsRaw: []string{"relay.example.com:443"}}
	if err := runConnect(f); err != nil {
		t.Fatalf("runConnect set: %v", err)
	}
	c, err := config.Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Server != "relay.example.com:443" || c.ServerPassword != "" {
		t.Errorf("set: server=%q pw=%q", c.Server, c.ServerPassword)
	}

	// Set host:port + password.
	f = &flags{connectArgsRaw: []string{"relay.example.com:443", "swordfish"}}
	if err := runConnect(f); err != nil {
		t.Fatalf("runConnect set+pw: %v", err)
	}
	c, _ = config.Load()
	if c.ServerPassword != "swordfish" {
		t.Errorf("pw not persisted: %q", c.ServerPassword)
	}

	// Default clears both.
	f = &flags{connectArgsRaw: []string{"default"}}
	if err := runConnect(f); err != nil {
		t.Fatalf("runConnect default: %v", err)
	}
	c, _ = config.Load()
	if c.Server != "" || c.ServerPassword != "" {
		t.Errorf("default did not clear: server=%q pw=%q", c.Server, c.ServerPassword)
	}
}

func TestRunConnect_RejectsBadHostPort(t *testing.T) {
	tmp := t.TempDir()
	config.SetPathForTesting(filepath.Join(tmp, "config.json"))
	t.Cleanup(func() { config.SetPathForTesting("") })

	// Bare hostname is now valid (defaults to :443) — use an input that
	// stays invalid: empty host with a port.
	f := &flags{connectArgsRaw: []string{":443"}}
	err := runConnect(f)
	if !errors.Is(err, fserrors.ErrUsage) {
		t.Errorf("bad input: got %v, want ErrUsage", err)
	}
}

// ---------------------------------------------------------------------------
// root.go small helpers
// ---------------------------------------------------------------------------

func TestValidMode(t *testing.T) {
	for _, ok := range []string{"", "local", "direct", "relay"} {
		if !validMode(ok) {
			t.Errorf("validMode(%q) = false, want true", ok)
		}
	}
	// The old colloquial names "stun"/"turn" must NOT be accepted now —
	// they were path-selection labels masquerading as protocol names and
	// were renamed for clarity. Catching them here keeps anyone who
	// re-adds them in the future from doing so silently.
	for _, bad := range []string{"udp", "lan", "internet", "anything", "stun", "turn"} {
		if validMode(bad) {
			t.Errorf("validMode(%q) = true, want false", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// main.go renderError + extractDetail
// ---------------------------------------------------------------------------

func TestExtractDetail(t *testing.T) {
	if got := extractDetail("usage error: bad flag"); got != "bad flag" {
		t.Errorf("got %q", got)
	}
	if got := extractDetail("no colon here"); got != "" {
		t.Errorf("got %q, want \"\"", got)
	}
}

func TestRenderError_MapsCancelledToUserCancelled(t *testing.T) {
	got := captureStderr(t, func() {
		_ = renderError(fmt.Errorf("wrapped: %w", io.EOF), false)
	})
	// Unknown wrap → catalog E099.
	if !strings.Contains(got, "E099") {
		t.Errorf("expected E099 catchall for unknown error, got: %s", got)
	}
}

// An unsendable symlink renders as a dedicated E036 with the offending path
// inlined and the --exclude hint, and exits 36.
func TestRenderError_UnsendableSymlink(t *testing.T) {
	err := fmt.Errorf("%w: broken link proj/dangling → ../gone (target does not exist)", fserrors.ErrUnsendableSymlink)
	var code int
	got := captureStderr(t, func() { code = renderError(err, false) })
	for _, want := range []string{
		"[E036] Cannot send a symlink: broken link proj/dangling → ../gone (target does not exist)",
		"Fix the link, or skip it with --exclude.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if code != 36 {
		t.Errorf("exit code = %d, want 36", code)
	}
}

// captureStderr redirects os.Stderr to a pipe for the duration of fn,
// then returns whatever was written. Used by tests of helpers that
// print to stderr (printPath, printCurrentServer, retryNoticeFor, ...).
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

// captureStdout is the stdout twin of captureStderr, for helpers whose
// output is a query answer (printCurrentServer's data lines).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	_ = w.Close()
	os.Stdout = old
	return <-done
}

// ---------------------------------------------------------------------------
// main.go debugRequested
// ---------------------------------------------------------------------------

func TestDebugRequested(t *testing.T) {
	savedArgs := os.Args
	savedEnv, hadEnv := os.LookupEnv("FSEND_DEBUG")
	t.Cleanup(func() {
		os.Args = savedArgs
		if hadEnv {
			_ = os.Setenv("FSEND_DEBUG", savedEnv)
		} else {
			_ = os.Unsetenv("FSEND_DEBUG")
		}
	})

	for _, tc := range []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{"none", "", []string{"fsend"}, false},
		{"env 1", "1", []string{"fsend"}, true},
		{"env true", "true", []string{"fsend"}, true},
		{"env 0", "0", []string{"fsend"}, false},
		{"env false", "false", []string{"fsend"}, false},
		{"flag", "", []string{"fsend", "--debug", "file.txt"}, true},
		{"flag past --", "", []string{"fsend", "--", "--debug"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == "" {
				_ = os.Unsetenv("FSEND_DEBUG")
			} else {
				_ = os.Setenv("FSEND_DEBUG", tc.env)
			}
			os.Args = tc.args
			if got := debugRequested(); got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// root.go applyEnvFallbacks
// ---------------------------------------------------------------------------

func TestApplyEnvFallbacks_FSEND_PASSWORD(t *testing.T) {
	saved, had := os.LookupEnv("FSEND_PASSWORD")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("FSEND_PASSWORD", saved)
		} else {
			_ = os.Unsetenv("FSEND_PASSWORD")
		}
	})

	c := rootCmd()
	_ = os.Setenv("FSEND_PASSWORD", "from-env")
	f := &flags{}
	applyEnvFallbacks(f, c)
	if f.passArg != "from-env" {
		t.Errorf("env not applied: %q", f.passArg)
	}

	// When the flag is already "changed", env is ignored — sender's
	// explicit --password must win.
	c2 := rootCmd()
	if err := c2.Flags().Set("password", "from-flag"); err != nil {
		t.Fatal(err)
	}
	f2 := &flags{passArg: "from-flag"}
	applyEnvFallbacks(f2, c2)
	if f2.passArg != "from-flag" {
		t.Errorf("flag should win: %q", f2.passArg)
	}
}

// ---------------------------------------------------------------------------
// internet.go signalingClient builder
// ---------------------------------------------------------------------------

func TestSignalingClient_SchemeSelection(t *testing.T) {
	// localhost → http://.
	cfg := &config.Config{Server: "127.0.0.1:8080"}
	c, addr := signalingClient(cfg)
	if c == nil || addr != "127.0.0.1:8080" {
		t.Fatalf("loopback: c=%v addr=%q", c, addr)
	}

	// Non-local → https://.
	cfg = &config.Config{Server: "fs.example.com:443"}
	if _, addr := signalingClient(cfg); addr != "fs.example.com:443" {
		t.Errorf("remote addr: %q", addr)
	}

	// Pre-prefixed URL is kept verbatim.
	cfg = &config.Config{Server: "https://fs.example.com:443"}
	_, addr = signalingClient(cfg)
	if addr != "https://fs.example.com:443" {
		t.Errorf("prefixed addr should be preserved: %q", addr)
	}
}

// ---------------------------------------------------------------------------
// connect.go printCurrentServer
// ---------------------------------------------------------------------------

func TestPrintCurrentServer_DefaultAndCustom(t *testing.T) {
	// The query's answer rides stdout (scriptable, `fsend --connect |
	// grep`); the guidance lines stay on stderr. Lock the split in.
	var guidance string
	answer := captureStdout(t, func() {
		guidance = captureStderr(t, func() {
			printCurrentServer(&config.Config{})
		})
	})
	if !strings.Contains(answer, "default") || !strings.Contains(answer, config.DefaultServer) {
		t.Errorf("default rendering missing markers on stdout:\n%s", answer)
	}
	if !strings.Contains(guidance, "Set a custom server") {
		t.Errorf("guidance missing from stderr:\n%s", guidance)
	}

	answer = captureStdout(t, func() {
		guidance = captureStderr(t, func() {
			printCurrentServer(&config.Config{Server: "relay.example.com:443", ServerPassword: "x"})
		})
	})
	if !strings.Contains(answer, "relay.example.com:443") || !strings.Contains(answer, "password set") {
		t.Errorf("custom rendering missing markers on stdout:\n%s", answer)
	}
	if !strings.Contains(guidance, "Revert to the default") {
		t.Errorf("guidance missing from stderr:\n%s", guidance)
	}
}

// ---------------------------------------------------------------------------
// internet.go retryNoticeFor
// ---------------------------------------------------------------------------

func TestRetryNoticeFor_QuietReturnsNil(t *testing.T) {
	if fn := retryNoticeFor(&flags{quiet: true}); fn != nil {
		t.Errorf("quiet mode should disable retry notice, got non-nil callback %p", fn)
	}
}

func TestRetryNoticeFor_PrintsAttempt(t *testing.T) {
	fn := retryNoticeFor(&flags{})
	if fn == nil {
		t.Fatal("non-quiet must return a callback")
	}
	got := captureStderr(t, func() {
		fn(2, time.Second, errors.New("transient: idle timeout"))
	})
	if !strings.Contains(got, "interrupted") || !strings.Contains(got, "2/") {
		t.Errorf("notice missing markers:\n%s", got)
	}
	// --debug mode includes the wrapped error in parentheses.
	debugGot := captureStderr(t, func() {
		retryNoticeFor(&flags{debug: true})(2, time.Second, errors.New("transient: idle timeout"))
	})
	if !strings.Contains(debugGot, "idle timeout") {
		t.Errorf("debug should expose underlying error:\n%s", debugGot)
	}
}

// ---------------------------------------------------------------------------
// send.go collectPlan
// ---------------------------------------------------------------------------

func TestCollectPlan_SingleFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(fp, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := collectPlan(&flags{}, []string{fp})
	if err != nil {
		t.Fatal(err)
	}
	if plan.mode != wire.ModeFiles || plan.label != "f.txt" || plan.totalBytes != 4 {
		t.Errorf("plan = %+v", plan)
	}
	if plan.totalFiles != 1 {
		t.Errorf("totalFiles = %d, want 1", plan.totalFiles)
	}
}

func TestCollectPlan_Directory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	if err := os.MkdirAll(filepath.Join(sub, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "nested", "g.txt"), []byte("yy"), 0o644); err != nil {
		t.Fatal(err)
	}
	plan, err := collectPlan(&flags{}, []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	if plan.mode != wire.ModeFiles || plan.label != "proj/" {
		t.Errorf("plan = %+v", plan)
	}
	if plan.totalFiles != 2 { // two regular files; dirs not counted
		t.Errorf("totalFiles = %d, want 2", plan.totalFiles)
	}
}

// Sending the contents of an empty folder is a no-op; fail fast rather than
// generate a code and wait on a receiver for nothing.
func TestCollectPlan_EmptyContentsErrors(t *testing.T) {
	dir := t.TempDir() // empty
	// Literal "<dir>/." (not filepath.Join, which would clean the "." away) —
	// this is what a shell passes for a contents send.
	_, err := collectPlan(&flags{}, []string{dir + "/."})
	if !errors.Is(err, fserrors.ErrUsage) {
		t.Fatalf("empty contents send: err = %v, want ErrUsage", err)
	}
	// But sending the empty folder itself (wrapped) is allowed — it recreates
	// the directory on the receiver.
	if _, err := collectPlan(&flags{}, []string{dir}); err != nil {
		t.Fatalf("sending an empty folder (wrapped) should be allowed: %v", err)
	}
}

// --exclude only applies when a directory is among the inputs.
func TestCollectPlan_ExcludeWithoutDirectory(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(fp, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		f     *flags
		paths []string
	}{
		{"plain_file", &flags{excludes: []string{"*.log"}}, []string{fp}},
		{"text", &flags{excludes: []string{"*.log"}, textArg: "hi"}, nil},
		{"stdin", &flags{excludes: []string{"*.log"}}, []string{"-"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collectPlan(tc.f, tc.paths)
			if !errors.Is(err, fserrors.ErrUsage) {
				t.Fatalf("err = %v, want ErrUsage", err)
			}
		})
	}
	if _, err := collectPlan(&flags{excludes: []string{"*.log"}}, []string{dir}); err != nil {
		t.Fatalf("directory with --exclude must not error: %v", err)
	}
}

// ---------------------------------------------------------------------------
// prompts.go readLine
// ---------------------------------------------------------------------------

func TestReadLine_BasicCases(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		eof      bool
	}{
		{"yes\n", "yes", false},
		{"  Y \n", "y", false},
		{"NO\r\n", "no", false},
		{"n", "n", false}, // unterminated final line is still an answer
		{"", "", true},    // closed stdin must be distinguishable from bare Enter
	} {
		got, eof := readLine(strings.NewReader(tc.in))
		if got != tc.want || eof != tc.eof {
			t.Errorf("readLine(%q) = (%q, %v), want (%q, %v)", tc.in, got, eof, tc.want, tc.eof)
		}
	}
}

// ---------------------------------------------------------------------------
// receive.go promptAccept under --yes / --quiet
// ---------------------------------------------------------------------------

func filesHello() wire.SenderHello {
	return wire.SenderHello{Mode: wire.ModeFiles, DisplayName: "proj/"}
}

// oneFileSummary is a realistic single-file classification for prompt tests
// that only care about chips/wording, not the file list (one file shows no
// preview rows). Carries OfferedBytes/Files so the headline renders sanely.
func oneFileSummary() transfer.ClassifySummary {
	const size = 1024
	return transfer.ClassifySummary{
		Total: 1, NewItems: 1, BytesToRecv: size, OfferedBytes: size,
		Files: []transfer.SummaryEntry{{RelativePath: "file.bin", Size: size, Status: "new", Type: wire.EntryFile}},
	}
}

func TestPromptAccept_QuietRequiresYes(t *testing.T) {
	h := filesHello()
	sum := transfer.ClassifySummary{Total: 1, NewItems: 1, BytesToRecv: 1}
	ui := newReceiverUI(context.Background(), &flags{quiet: true}, "/tmp", false, mustLANInfo())
	if ui.promptAccept(h, sum) {
		t.Error("quiet without --yes must decline")
	}
	ui = newReceiverUI(context.Background(), &flags{quiet: true, yes: true}, "/tmp", false, mustLANInfo())
	if !ui.promptAccept(h, sum) {
		t.Error("quiet + --yes must accept")
	}
}

func TestPromptAccept_YesAccepts(t *testing.T) {
	for _, h := range []wire.SenderHello{
		filesHello(),
		{Mode: wire.ModeStream, IsText: true, DisplayName: "msg.txt"},
		{Mode: wire.ModeStream, DisplayName: "stdin"},
	} {
		got := captureStderr(t, func() {
			ui := newReceiverUI(context.Background(), &flags{yes: true}, "/tmp", false, mustLANInfo())
			if !ui.promptAccept(h, oneFileSummary()) {
				t.Errorf("--yes must accept hello %+v", h)
			}
		})
		if !strings.Contains(got, "Incoming") {
			t.Errorf("prompt block missing:\n%s", got)
		}
	}
}

// The headline reconciles the offered total (agrees with the sender) with
// what will actually transfer ("X of Y") and surfaces the conflict count,
// which must survive even when the differing files are too small to appear in
// the size-ranked preview rows.
func TestPromptAccept_HeadlineReconcilesOfferedAndNet(t *testing.T) {
	got := captureStderr(t, func() {
		ui := newReceiverUI(context.Background(), &flags{yes: true}, "/tmp", false, mustLANInfo())
		_ = ui.promptAccept(filesHello(), transfer.ClassifySummary{
			Total: 10, NewItems: 3, Identical: 6, Differing: 1,
			OfferedBytes: 1_200_000_000, BytesToRecv: 307_000_000, DifferingBytes: 5,
			Files: []transfer.SummaryEntry{
				{RelativePath: "a.bin", Size: 900_000_000, Status: "new", Type: wire.EntryFile},
				{RelativePath: "b.bin", Size: 307_000_000, Status: "new", Type: wire.EntryFile},
			},
		})
	})
	for _, want := range []string{"307 MB of 1.2 GB", "1 differ"} {
		if !strings.Contains(got, want) {
			t.Errorf("headline missing %q:\n%s", want, got)
		}
	}
	// The old stacked breakdown line must be gone.
	if strings.Contains(got, "3 new") || strings.Contains(got, "6 up to date") {
		t.Errorf("stacked breakdown line should be removed:\n%s", got)
	}
}

func TestSummaryParts_ResumeShowsMovedAndHonestRate(t *testing.T) {
	// 200 MB total, 50 MB moved in 1s → size annotated with the moved
	// clause and the rate computed from moved, not total.
	total, moved := int64(200_000_000), int64(50_000_000)
	parts := strings.Join(summaryParts(total, moved, "sent", time.Second, mustLANInfo()), "  ·  ")
	if !strings.Contains(parts, "200 MB (50 MB sent)") {
		t.Errorf("missing moved clause: %s", parts)
	}
	if !strings.Contains(parts, "50 MB/s") {
		t.Errorf("rate must derive from moved bytes: %s", parts)
	}
	// Non-resumed: no annotation, rate from the full size.
	parts = strings.Join(summaryParts(total, total, "sent", time.Second, mustLANInfo()), "  ·  ")
	if strings.Contains(parts, "(") || !strings.Contains(parts, "200 MB/s") {
		t.Errorf("non-resumed summary changed shape: %s", parts)
	}
}

func TestPromptAccept_PasswordChipRendered(t *testing.T) {
	got := captureStderr(t, func() {
		h := wire.SenderHello{Mode: wire.ModeFiles, DisplayName: "proj/", HasPassword: true}
		ui := newReceiverUI(context.Background(), &flags{yes: true}, "/tmp", false, mustLANInfo())
		_ = ui.promptAccept(h, oneFileSummary())
	})
	if !strings.Contains(got, "password required") {
		t.Errorf("expected password chip, got:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// internet.go printPath
// ---------------------------------------------------------------------------

func TestPrintPath_QuietSuppressed(t *testing.T) {
	// Build a minimal connpath.Info via the public constructor.
	got := captureStderr(t, func() {
		printPath(&flags{quiet: true}, mustLANInfo())
	})
	if got != "" {
		t.Errorf("--quiet should suppress, got %q", got)
	}
}

// The path now rides inline on the connected/incoming lines, so the
// standalone headline is --debug only — even for relay.
func TestPrintPath_DebugOnly(t *testing.T) {
	relay := connpath.FromRelay("relay.example.com:443")
	if got := captureStderr(t, func() { printPath(&flags{}, relay) }); got != "" {
		t.Errorf("non-debug should print nothing, got %q", got)
	}
	got := captureStderr(t, func() { printPath(&flags{debug: true}, relay) })
	if !strings.Contains(got, "Relayed via relay.example.com:443") {
		t.Errorf("debug headline missing:\n%s", got)
	}
	got = captureStderr(t, func() { printPath(&flags{debug: true}, connpath.FromICE("srflx", "host")) })
	if !strings.Contains(got, "ICE candidate pair: srflx → host") {
		t.Errorf("debug ICE detail missing:\n%s", got)
	}
}

// Route transparency: the accept prompt names the path for every kind.
func TestPromptAccept_PathChipShown(t *testing.T) {
	cases := []struct {
		info connpath.Info
		want string
	}{
		{connpath.FromLAN(), "local network"},
		{connpath.FromICE("srflx", "host"), "direct over the internet"},
		{connpath.FromRelay("relay.example.com:443"), "relayed via relay.example.com:443"},
	}
	for _, c := range cases {
		got := captureStderr(t, func() {
			ui := newReceiverUI(context.Background(), &flags{yes: true}, "/tmp", false, c.info)
			_ = ui.promptAccept(filesHello(), oneFileSummary())
		})
		if !strings.Contains(got, "Incoming from") || !strings.Contains(got, c.want) {
			t.Errorf("prompt missing path chip %q:\n%s", c.want, got)
		}
	}
}
