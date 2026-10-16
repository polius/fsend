package transfer

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// nopRWC is a Streams.Control sink that discards frames; enough for
// guardRegular's fail-closed declineTransfer call.
type nopRWC struct{}

func (nopRWC) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopRWC) Close() error                { return nil }

// TestGuardRegular covers the B2 uniform symlink guard: a planted symlink or
// directory where the sidecar/target should be is refused (wrapping
// ErrWriteFailed), a regular file and an absent path are allowed.
func TestGuardRegular(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	dir := t.TempDir()
	s := &Streams{Control: nopRWC{}}

	reg := filepath.Join(dir, "regular")
	if err := os.WriteFile(reg, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := guardRegular(s, reg, "target"); err != nil {
		t.Errorf("regular file rejected: %v", err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(reg, link); err != nil {
		t.Fatal(err)
	}
	if err := guardRegular(s, link, "target"); !errors.Is(err, fserrors.ErrWriteFailed) {
		t.Errorf("symlink err = %v, want ErrWriteFailed", err)
	}

	if err := guardRegular(s, dir, "target"); !errors.Is(err, fserrors.ErrWriteFailed) {
		t.Errorf("directory err = %v, want ErrWriteFailed", err)
	}

	if err := guardRegular(s, filepath.Join(dir, "absent"), "target"); err != nil {
		t.Errorf("absent path rejected: %v", err)
	}
}
