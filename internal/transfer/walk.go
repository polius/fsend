package transfer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/wire"
)

// SourceItem is one item to be sent: a file or symlink, with the
// metadata the wire protocol needs.
type SourceItem struct {
	Info      wire.FileInfo // wire payload (RelativePath, Size, Mode, ModTime, IsSymlink, SymlinkTarget, Blake3Root)
	AbsPath   string        // sender-local absolute path; empty for synthetic items (stdin, text)
	Reader    io.Reader     // non-nil only for synthetic items (stdin, text)
	Resumable bool
}

// Walk expands a list of CLI arguments into the ordered SourceItem list the
// wire protocol will carry.
//
// Directories are not supported here — the CLI bundles directories into a
// tar via BuildArchive before calling Send. Walk only handles regular
// files and symlinks (one entry per argument, in argument order).
func Walk(paths []string) ([]SourceItem, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("walk: no paths provided")
	}

	items := make([]SourceItem, 0, len(paths))
	for i, raw := range paths {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		base := filepath.Base(abs)

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(abs)
			items = append(items, SourceItem{
				Info: wire.FileInfo{
					Index:         uint32(i),
					RelativePath:  base,
					Mode:          uint32(info.Mode().Perm()),
					ModTime:       info.ModTime().UnixNano(),
					IsSymlink:     true,
					SymlinkTarget: target,
					Resumable:     false,
				},
				AbsPath: abs,
			})
		case info.IsDir():
			return nil, fmt.Errorf("walk: %s is a directory — directories must be sent through BuildArchive", raw)
		default:
			hash, err := blake3FileHash(abs)
			if err != nil {
				return nil, err
			}
			items = append(items, SourceItem{
				Info: wire.FileInfo{
					Index:        uint32(i),
					RelativePath: base,
					Size:         uint64(info.Size()),
					Mode:         uint32(info.Mode().Perm()),
					ModTime:      info.ModTime().UnixNano(),
					Blake3Root:   hash,
					Resumable:    true,
				},
				AbsPath:   abs,
				Resumable: true,
			})
		}
	}
	return items, nil
}

// blake3FileHash streams the file once and returns the 32-byte BLAKE3 root.
func blake3FileHash(path string) ([32]byte, error) {
	var zero [32]byte
	f, err := os.Open(path)
	if err != nil {
		return zero, fmt.Errorf("walk: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := blake3.New()
	if _, err := io.Copy(h, f); err != nil {
		return zero, fmt.Errorf("walk: hashing %s: %w", path, err)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

// SanitizeRelativePath validates an incoming RelativePath for safety on
// the receiver side. Rejects:
//   - absolute paths (Unix style or Windows style)
//   - paths containing .. segments
//   - paths with Windows drive letters / volume names (even when running on
//     non-Windows, since the bytes can still arrive over the wire)
//   - empty paths
//
// Returns the cleaned, OS-native-separator form on success.
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
	// Explicitly reject Windows-style drive letters (e.g. "C:\..." or "C:foo")
	// even when we're not on Windows. filepath.VolumeName is a no-op on Unix,
	// so we do this check manually.
	if len(p) >= 2 && p[1] == ':' {
		c := p[0]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') {
			return "", fmt.Errorf("windows drive-letter path rejected: %q", p)
		}
	}
	// Reject UNC-style "\\server\share\..." paths.
	if strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//") {
		return "", fmt.Errorf("UNC-style path rejected: %q", p)
	}
	// Normalize separators for the host OS.
	clean := filepath.Clean(filepath.FromSlash(p))
	// Reject if clean contains .. as a path component.
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		if part == ".." {
			return "", fmt.Errorf("path traversal rejected: %q", p)
		}
	}
	if vol := filepath.VolumeName(clean); vol != "" {
		return "", fmt.Errorf("volume name rejected: %q", p)
	}
	return clean, nil
}
