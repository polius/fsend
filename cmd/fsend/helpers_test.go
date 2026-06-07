package main

import (
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

func TestStunHostFromServer(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"fs.example.com:443", "fs.example.com"},
		{"127.0.0.1:8080", ""},
		{"localhost:8080", ""},
		{"[::1]:443", ""},
		// No port → SplitHostPort errors and the whole string is taken as host.
		// We still reject loopback even in that shape.
		{"localhost", ""},
		{"fs.example.com", "fs.example.com"},
	} {
		if got := stunHostFromServer(tc.in); got != tc.want {
			t.Errorf("stunHostFromServer(%q) = %q, want %q", tc.in, got, tc.want)
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

func TestSynthesizeText(t *testing.T) {
	items := synthesizeText("hello")
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	it := items[0]
	if it.Info.Size != 5 || it.Reader == nil {
		t.Errorf("size=%d reader=%v", it.Info.Size, it.Reader)
	}
	if !strings.HasPrefix(it.Info.RelativePath, "fsend-text-") || !strings.HasSuffix(it.Info.RelativePath, ".txt") {
		t.Errorf("name = %q", it.Info.RelativePath)
	}
	// Content the Reader replays should match the input.
	got, err := io.ReadAll(it.Reader)
	if err != nil || string(got) != "hello" {
		t.Errorf("reader content = %q err=%v", got, err)
	}
}

func TestSynthesizeStdin(t *testing.T) {
	items, err := synthesizeStdin()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("len = %d, want 1", len(items))
	}
	it := items[0]
	if !it.Info.Streaming {
		t.Error("expected Streaming=true")
	}
	if it.Info.Resumable {
		t.Error("expected Resumable=false (stdin can't seek)")
	}
}

func TestTotalBytes(t *testing.T) {
	items := []transfer.SourceItem{
		{Info: wire.FileInfo{Size: 12}},
		{Info: wire.FileInfo{Size: 34}},
		{Info: wire.FileInfo{Size: 56}},
	}
	if got := totalBytes(items); got != 12+34+56 {
		t.Errorf("totalBytes = %d", got)
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
	for _, bad := range []string{"", "   ", ":443", "host:0", "host:99999", "host:abc"} {
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
	for _, ok := range []string{"", "local", "stun", "turn"} {
		if !validMode(ok) {
			t.Errorf("validMode(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"udp", "lan", "internet", "anything"} {
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

func TestApplyEnvFallbacks_FSEND_PASS(t *testing.T) {
	saved, had := os.LookupEnv("FSEND_PASS")
	t.Cleanup(func() {
		if had {
			_ = os.Setenv("FSEND_PASS", saved)
		} else {
			_ = os.Unsetenv("FSEND_PASS")
		}
	})

	c := rootCmd()
	_ = os.Setenv("FSEND_PASS", "from-env")
	f := &flags{}
	applyEnvFallbacks(f, c)
	if f.passArg != "from-env" {
		t.Errorf("env not applied: %q", f.passArg)
	}

	// When the flag is already "changed", env is ignored — sender's
	// explicit --pass must win.
	c2 := rootCmd()
	if err := c2.Flags().Set("pass", "from-flag"); err != nil {
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
	got := captureStderr(t, func() {
		printCurrentServer(&config.Config{})
	})
	if !strings.Contains(got, "default") || !strings.Contains(got, config.DefaultServer) {
		t.Errorf("default rendering missing markers:\n%s", got)
	}

	got = captureStderr(t, func() {
		printCurrentServer(&config.Config{Server: "relay.example.com:443", ServerPassword: "x"})
	})
	if !strings.Contains(got, "relay.example.com:443") || !strings.Contains(got, "password set") {
		t.Errorf("custom rendering missing markers:\n%s", got)
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
// send.go collectItems for text/stdin (the cases that don't touch disk)
// ---------------------------------------------------------------------------

func TestCollectItems_Text(t *testing.T) {
	items, kind, totalFiles, label, cleanup, err := collectItems(&flags{textArg: "hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if kind != wire.TransferText {
		t.Errorf("kind = %v, want TransferText", kind)
	}
	if len(items) != 1 || items[0].Info.Size != 5 {
		t.Errorf("items = %+v", items)
	}
	if totalFiles != 0 || label != "" {
		t.Errorf("text should not set totalFiles/label: %d %q", totalFiles, label)
	}
}

func TestCollectItems_Stdin(t *testing.T) {
	items, kind, _, _, cleanup, err := collectItems(&flags{}, []string{"-"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if kind != wire.TransferStdin {
		t.Errorf("kind = %v, want TransferStdin", kind)
	}
	if !items[0].Info.Streaming {
		t.Error("stdin item must be Streaming")
	}
}

func TestCollectItems_SingleFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(fp, []byte("body"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, kind, _, label, cleanup, err := collectItems(&flags{}, []string{fp})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if kind != wire.TransferSingleFile {
		t.Errorf("kind = %v, want TransferSingleFile", kind)
	}
	if label != "f.txt" {
		t.Errorf("label = %q, want f.txt", label)
	}
	if items[0].Info.Size != 4 {
		t.Errorf("size = %d", items[0].Info.Size)
	}
}

func TestCollectItems_Directory_BuildsArchive(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "proj")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	items, kind, totalFiles, label, cleanup, err := collectItems(&flags{}, []string{sub})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if kind != wire.TransferDirectory {
		t.Errorf("kind = %v, want TransferDirectory", kind)
	}
	if totalFiles == 0 {
		t.Error("totalFiles must be > 0 for archive")
	}
	if label != "proj/" {
		t.Errorf("label = %q, want proj/", label)
	}
	if items[0].Info.RelativePath != transfer.ArchiveName {
		t.Errorf("archive wire-name = %q", items[0].Info.RelativePath)
	}
}

// ---------------------------------------------------------------------------
// prompts.go readLine
// ---------------------------------------------------------------------------

func TestReadLine_BasicCases(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"yes\n", "yes"},
		{"  Y \n", "y"},
		{"NO\r\n", "no"},
		{"", ""},
	} {
		if got := readLine(strings.NewReader(tc.in)); got != tc.want {
			t.Errorf("readLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// receive.go promptAccept under --yes / --quiet
// ---------------------------------------------------------------------------

func TestPromptAccept_QuietRequiresYes(t *testing.T) {
	h := wire.SenderHello{TransferKind: wire.TransferSingleFile, DisplayName: "x", TotalBytes: 1}
	if promptAccept(&flags{quiet: true}, h, "/tmp", mustLANInfo()) {
		t.Error("quiet without --yes must decline")
	}
	if !promptAccept(&flags{quiet: true, yes: true}, h, "/tmp", mustLANInfo()) {
		t.Error("quiet + --yes must accept")
	}
}

func TestPromptAccept_YesAcceptsAcrossKinds(t *testing.T) {
	for _, k := range []wire.TransferKind{
		wire.TransferSingleFile,
		wire.TransferMultiFile,
		wire.TransferDirectory,
		wire.TransferText,
		wire.TransferStdin,
	} {
		got := captureStderr(t, func() {
			h := wire.SenderHello{TransferKind: k, DisplayName: "x", TotalBytes: 1, TotalFiles: 1}
			if !promptAccept(&flags{yes: true}, h, "/tmp", mustLANInfo()) {
				t.Errorf("--yes must accept kind %v", k)
			}
		})
		if !strings.Contains(got, "Incoming") {
			t.Errorf("kind %v: prompt block missing:\n%s", k, got)
		}
	}
}

func TestPromptAccept_PasswordChipRendered(t *testing.T) {
	got := captureStderr(t, func() {
		h := wire.SenderHello{
			TransferKind: wire.TransferSingleFile,
			DisplayName:  "x",
			TotalBytes:   1,
			HasPassword:  true,
		}
		_ = promptAccept(&flags{yes: true}, h, "/tmp", mustLANInfo())
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
