//go:build unix

package transfer

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// A fifo named directly as an argument is refused honestly (E024 naming the
// kind), not misreported as an empty folder or a missing path.
func TestWalk_FifoArgErrors(t *testing.T) {
	dir := t.TempDir()
	pipe := filepath.Join(dir, "pipe.f")
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	_, err := Walk([]string{pipe}, nil)
	if !errors.Is(err, fserrors.ErrUsage) {
		t.Fatalf("want ErrUsage, got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "not a regular file (fifo)") {
		t.Errorf("error should name the kind: %q", got)
	}
}

// Same for a unix socket.
func TestWalk_SocketArgErrors(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock.s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix sockets unsupported here: %v", err)
	}
	defer l.Close()

	_, err = Walk([]string{sock}, nil)
	if !errors.Is(err, fserrors.ErrUsage) {
		t.Fatalf("want ErrUsage, got %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "not a regular file (socket)") {
		t.Errorf("error should name the kind: %q", got)
	}
}

// A fifo inside a walked folder keeps the long-standing silent skip.
func TestWalk_FifoInsideFolderSkipped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe.f"), 0o644); err != nil {
		t.Skipf("mkfifo unsupported here: %v", err)
	}

	srcs, err := Walk([]string{dir}, nil)
	if err != nil {
		t.Fatalf("fifo inside a folder must not fail the walk: %v", err)
	}
	if _, ok := bySrcRel(srcs)[filepath.Base(dir)+"/pipe.f"]; ok {
		t.Error("fifo should be skipped, not listed")
	}
}

// An unreadable file argument fails at walk (plan) time, before any code is
// issued, with a permission error the CLI maps to E010.
func TestWalk_UnreadableFileArgErrors(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	dir := t.TempDir()
	locked := filepath.Join(dir, "locked.txt")
	if err := os.WriteFile(locked, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}

	_, err := Walk([]string{locked}, nil)
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("want a permission error, got %v", err)
	}
}
