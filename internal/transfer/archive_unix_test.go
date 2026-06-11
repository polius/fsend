//go:build unix

package transfer

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A FIFO inside a sent folder used to hang BuildArchive forever
// (os.Open on a fifo blocks until a writer appears — before the share
// code was even printed); a socket aborted the whole send. Both must be
// skipped. syscall.Mkfifo only exists on unix, so this lives in a
// build-tagged file — go vet/test on windows can't compile it otherwise.
func TestBuildArchive_SkipsSpecialFiles(t *testing.T) {
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "real.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(srcDir, "pipe.fifo"), 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	done := make(chan struct{})
	var arc *ArchiveResult
	var err error
	go func() {
		arc, err = BuildArchive([]string{srcDir}, nil)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("BuildArchive hung on the fifo")
	}
	if err != nil {
		t.Fatalf("BuildArchive: %v", err)
	}
	defer os.Remove(arc.Path)
	if arc.NumFiles != 1 {
		t.Errorf("NumFiles = %d, want 1 (fifo skipped)", arc.NumFiles)
	}
}
