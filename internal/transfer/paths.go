package transfer

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/polius/fsend/internal/fserrors"
)

// excludeMatcher matches a forward-slash relative path against glob patterns
// (filepath.Match semantics; no `**`). A pattern matches if it equals the
// full relative path or any single path component.
type excludeMatcher struct {
	patterns []string
}

// newExcludeMatcher trims/validates patterns; an empty pattern is dropped so
// `--exclude ""` is a no-op rather than a match-everything bomb.
func newExcludeMatcher(patterns []string) (excludeMatcher, error) {
	out := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if _, err := filepath.Match(p, ""); err != nil {
			return excludeMatcher{}, fmt.Errorf("%w: invalid --exclude pattern %q: %v", fserrors.ErrUsage, p, err)
		}
		out = append(out, p)
	}
	return excludeMatcher{patterns: out}, nil
}

func (m excludeMatcher) match(rel string) bool {
	if len(m.patterns) == 0 {
		return false
	}
	rel = filepath.ToSlash(rel)
	parts := strings.Split(rel, "/")
	for _, pat := range m.patterns {
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
		for _, part := range parts {
			if ok, _ := filepath.Match(pat, part); ok {
				return true
			}
		}
	}
	return false
}

// SanitizeRelativePath validates a peer-supplied RelativePath and returns the
// cleaned, OS-native form. Rejects absolute paths, .. segments, Windows drive
// letters, UNC paths, and empties — even when not running on Windows, since
// the bytes still arrive over the wire.
func SanitizeRelativePath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`) {
		return "", fmt.Errorf("absolute path rejected: %q", p)
	}
	if filepath.IsAbs(p) {
		return "", fmt.Errorf("absolute path rejected: %q", p)
	}
	if len(p) >= 2 && p[1] == ':' {
		if c := p[0]; (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return "", fmt.Errorf("windows drive-letter path rejected: %q", p)
		}
	}
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("UNC-style path rejected: %q", p)
	}
	clean := filepath.Clean(filepath.FromSlash(p))
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("path traversal rejected: %q", p)
		}
	}
	if filepath.VolumeName(clean) != "" {
		return "", fmt.Errorf("volume name rejected: %q", p)
	}
	return clean, nil
}
