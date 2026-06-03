package transfer

import (
	"path/filepath"
	"strings"
)

// pathIsUnder reports whether child is the same as parent or lives below it
// in the filesystem hierarchy. Both must be absolute and clean.
func pathIsUnder(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}
