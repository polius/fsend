package transfer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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

// A file the user creates at a target after it was classified as absent is
// theirs: the incoming file is not placed over it; the entry is skipped with
// a reason and the rest arrive.
func TestEngine_FileAppearedDuringTransferIsKept(t *testing.T) {
	dst := t.TempDir()
	var skipped []string
	_, re := runTransfer(t,
		SendOptions{Mode: wire.ModeFiles, Sources: sourcesFor(t, []string{"a.txt", "b.txt"})},
		RecvOptions{TargetDir: dst,
			// Runs after classification, before any byte lands.
			Accept: func(wire.SenderHello, ClassifySummary) bool {
				writeFile(t, filepath.Join(dst, "b.txt"), []byte("made meanwhile"))
				return true
			},
			OnEntrySkipped: func(rel, _ string) { skipped = append(skipped, rel) }})
	if re != nil {
		t.Fatalf("recv = %v", re)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "b.txt")); string(b) != "made meanwhile" {
		t.Errorf("b.txt = %q: the file made during the transfer was replaced", b)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "content of 000" {
		t.Errorf("a.txt = %q, want it delivered", b)
	}
	if len(skipped) != 1 || skipped[0] != "b.txt" {
		t.Errorf("skipped = %v, want [b.txt]", skipped)
	}
}

// A Ctrl-C reaches a control write blocked on a peer that stopped reading
// (a large listing upload), not just reads.
func TestBindCtx_CancelUnblocksControlWrite(t *testing.T) {
	a, b := tcpPair(t)
	defer func() { _ = a.Close(); _ = b.Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	s, stop := bindCtx(ctx, &Streams{Control: a, Data: a})
	defer stop()
	errc := make(chan error, 1)
	go func() {
		_, err := s.Control.Write(make([]byte, 256<<20)) // far past the socket buffers
		errc <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("write err = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a cancelled context left the control write blocked")
	}
}

// A broken link inside a folder is skipped and reported; one named on the
// command line is still refused (E036).
func TestWalkReport_SkipsDanglingLinksInsideFolders(t *testing.T) {
	root := filepath.Join(t.TempDir(), "app")
	writeFile(t, filepath.Join(root, "src", "index.js"), []byte("x"))
	if err := os.MkdirAll(filepath.Join(root, "node_modules", ".bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "node_modules", ".bin", "cli")
	if err := os.Symlink("../gone/cli.js", link); err != nil {
		t.Fatal(err)
	}
	srcs, skips, err := WalkReport([]string{root}, nil)
	if err != nil {
		t.Fatalf("WalkReport = %v", err)
	}
	if len(skips) != 1 || skips[0].Path != "app/node_modules/.bin/cli" || !strings.Contains(skips[0].Reason, "../gone/cli.js") {
		t.Errorf("skips = %+v", skips)
	}
	for _, s := range srcs {
		if strings.HasSuffix(s.Entry.RelativePath, "/cli") {
			t.Errorf("the broken link is still in the listing")
		}
	}
	if _, _, err := WalkReport([]string{link}, nil); !errors.Is(err, fserrors.ErrUnsendableSymlink) {
		t.Errorf("a broken link named directly = %v, want E036", err)
	}
}
