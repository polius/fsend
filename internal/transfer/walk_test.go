package transfer

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/polius/fsend/internal/fserrors"
)

// TestWalk_RejectsDuplicateBaseNames guards the up-front collision check:
// receivers place every item by its base name, so two arguments sharing
// one (even by case, for macOS/Windows receivers) must fail before any
// byte moves instead of clobbering mid-transfer.
func TestWalk_RejectsDuplicateBaseNames(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a", "report.pdf")
	b := filepath.Join(dir, "b", "Report.pdf")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Walk([]string{a, b}); !errors.Is(err, fserrors.ErrUsage) {
		t.Fatalf("Walk with duplicate base names: got %v, want ErrUsage", err)
	}
	if _, err := Walk([]string{a}); err != nil {
		t.Fatalf("Walk with a single path: %v", err)
	}
}
