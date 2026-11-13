package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// sourcesFor builds a listing whose wire names are names (any bytes) while
// the content comes from real files under dir — so a test can offer names
// this machine could never hold itself.
func sourcesFor(t *testing.T, names []string) []Source {
	t.Helper()
	dir := t.TempDir()
	var out []Source
	for i, n := range names {
		p := filepath.Join(dir, "src"+pad(i))
		writeFile(t, p, []byte("content of "+pad(i)))
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, Source{AbsPath: p, Entry: wire.ListingEntry{
			Index: uint32(i), RelativePath: n, Size: uint64(st.Size()),
			ModTimeSec: st.ModTime().Unix(), Mode: 0o644, Type: wire.EntryFile,
		}})
	}
	return out
}

type refusals struct {
	mu   sync.Mutex
	recv map[string]string
	send map[uint32]string
}

func runRefusing(t *testing.T, names []string, dst string) (*refusals, error, error) {
	t.Helper()
	got := &refusals{recv: map[string]string{}, send: map[uint32]string{}}
	se, re := runTransfer(t,
		SendOptions{Mode: wire.ModeFiles, Sources: sourcesFor(t, names),
			OnRefused: func(i uint32, why string) { got.mu.Lock(); got.send[i] = why; got.mu.Unlock() }},
		RecvOptions{TargetDir: dst,
			OnEntrySkipped: func(rel, why string) { got.mu.Lock(); got.recv[rel] = why; got.mu.Unlock() }})
	return got, se, re
}

// A name Windows cannot create costs that one file — skipped with a reason
// on both sides — never the transfer, and is not reported as an attack.
func TestEngine_WindowsUnsafeNamesSkipOneFile(t *testing.T) {
	defer func(o string) { hostOS = o }(hostOS)
	hostOS = "windows"
	dst := t.TempDir()
	bad := []string{"linux/drivers/aux.c", "backup-12:30:00.tar", "What now?.txt", "v1.", "con.txt"}
	got, se, re := runRefusing(t, append([]string{"ok.txt"}, bad...), dst)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v, want both to finish", se, re)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "ok.txt")); string(b) != "content of 000" {
		t.Errorf("ok.txt = %q, want it delivered", b)
	}
	for i, n := range bad {
		if got.recv[n] == "" {
			t.Errorf("receiver did not list %q as skipped (got %v)", n, got.recv)
		}
		if got.send[uint32(i+1)] == "" {
			t.Errorf("sender did not hear why %q was refused (got %v)", n, got.send)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, ".fsend")); err == nil {
		t.Error(".fsend was left behind")
	}
}

// E012 stays for real traversal attempts.
func TestEngine_TraversalStillRejected(t *testing.T) {
	defer func(o string) { hostOS = o }(hostOS)
	for _, goos := range []string{"windows", "darwin"} {
		hostOS = goos
		_, _, re := runRefusing(t, []string{"ok.txt", "../escape.txt"}, t.TempDir())
		if !errors.Is(re, fserrors.ErrPathTraversal) {
			t.Errorf("%s: recv = %v, want E012", goos, re)
		}
	}
}

// A name the target filesystem refuses at commit (too long everywhere;
// invalid UTF-8 on APFS) skips that file; its neighbours land and .fsend is
// cleaned up.
func TestEngine_NameTheFilesystemRefuses(t *testing.T) {
	dst := t.TempDir()
	long := strings.Repeat("x", 300) + ".txt"
	got, se, re := runRefusing(t, []string{"good.txt", long, "bad\xff.txt", "after.txt"}, dst)
	if se != nil || re != nil {
		t.Fatalf("send=%v recv=%v, want both to finish", se, re)
	}
	for _, n := range []string{"good.txt", "after.txt"} {
		if _, err := os.Stat(filepath.Join(dst, n)); err != nil {
			t.Errorf("%s did not arrive: %v", n, err)
		}
	}
	if got.recv[long] == "" {
		t.Errorf("the over-long name was not listed as skipped: %v", got.recv)
	}
	if _, err := os.Stat(filepath.Join(dst, ".fsend")); err == nil {
		t.Error(".fsend was left behind")
	}
}
