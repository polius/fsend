package transfer

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/polius/fsend/internal/wire"
)

// SourceItem is one item to be sent: a file, directory, or symlink, with
// the metadata the wire protocol needs.
type SourceItem struct {
	Info     wire.FileInfo // wire payload (RelativePath, Size, Mode, ModTime, IsDir, IsSymlink, SymlinkTarget, Blake3Root)
	AbsPath  string        // sender-local absolute path; empty for synthetic items (stdin, text)
	Reader   io.Reader     // non-nil only for synthetic items (stdin, text)
	Resumable bool
}

// Walk expands a list of CLI arguments into the ordered SourceItem list the
// wire protocol will carry.
//
// For each top-level argument:
//   - If it's a directory, the directory and its descendants are emitted
//     in deterministic (lexicographic) order. The directory's own basename
//     becomes the leading component of every RelativePath under it.
//   - If it's a regular file, one SourceItem is emitted whose RelativePath
//     is just the basename.
//   - Symlinks are recorded as symlinks; we do NOT follow them.
//
// BLAKE3 roots are computed for regular files in a single pass each. For
// large trees this is the slowest step pre-transfer; we accept the cost
// because it gives us cryptographic resume validation.
func Walk(paths []string) ([]SourceItem, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("walk: no paths provided")
	}

	var items []SourceItem
	var index uint32

	for _, raw := range paths {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		info, err := os.Lstat(abs)
		if err != nil {
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		root := filepath.Base(abs)

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(abs)
			items = append(items, SourceItem{
				Info: wire.FileInfo{
					Index:         index,
					RelativePath:  root,
					Mode:          uint32(info.Mode().Perm()),
					ModTime:       info.ModTime().UnixNano(),
					IsSymlink:     true,
					SymlinkTarget: target,
					Resumable:     false,
				},
				AbsPath: abs,
			})
			index++

		case info.IsDir():
			subItems, err := walkDir(abs, root, &index)
			if err != nil {
				return nil, err
			}
			items = append(items, subItems...)

		default:
			hash, err := blake3FileHash(abs)
			if err != nil {
				return nil, err
			}
			items = append(items, SourceItem{
				Info: wire.FileInfo{
					Index:        index,
					RelativePath: root,
					Size:         uint64(info.Size()),
					Mode:         uint32(info.Mode().Perm()),
					ModTime:      info.ModTime().UnixNano(),
					Blake3Root:   hash,
					Resumable:    true,
				},
				AbsPath:   abs,
				Resumable: true,
			})
			index++
		}
	}
	return items, nil
}

// walkDir emits one SourceItem for the directory itself, then recursively
// for every entry below it, in lexicographic order.
func walkDir(absRoot, relRoot string, indexCounter *uint32) ([]SourceItem, error) {
	var items []SourceItem

	// First, the directory itself.
	dirInfo, err := os.Lstat(absRoot)
	if err != nil {
		return nil, fmt.Errorf("walk: stat %s: %w", absRoot, err)
	}
	items = append(items, SourceItem{
		Info: wire.FileInfo{
			Index:        *indexCounter,
			RelativePath: relRoot,
			Mode:         uint32(dirInfo.Mode().Perm()),
			ModTime:      dirInfo.ModTime().UnixNano(),
			IsDir:        true,
			Resumable:    false,
		},
		AbsPath: absRoot,
	})
	*indexCounter++

	// Then everything under it.
	err = filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// Skip entries the OS rejects; warn would be nicer but we don't
			// have a logger here yet — the calling layer can hook in later.
			return nil
		}
		if path == absRoot {
			return nil // already emitted above
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// Compute relative path with forward slashes (wire convention).
		rel, err := filepath.Rel(absRoot, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(filepath.Join(relRoot, rel))

		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			items = append(items, SourceItem{
				Info: wire.FileInfo{
					Index:         *indexCounter,
					RelativePath:  rel,
					Mode:          uint32(info.Mode().Perm()),
					ModTime:       info.ModTime().UnixNano(),
					IsSymlink:     true,
					SymlinkTarget: target,
					Resumable:     false,
				},
				AbsPath: path,
			})
			*indexCounter++

		case info.IsDir():
			items = append(items, SourceItem{
				Info: wire.FileInfo{
					Index:        *indexCounter,
					RelativePath: rel,
					Mode:         uint32(info.Mode().Perm()),
					ModTime:      info.ModTime().UnixNano(),
					IsDir:        true,
					Resumable:    false,
				},
				AbsPath: path,
			})
			*indexCounter++

		default:
			hash, err := blake3FileHash(path)
			if err != nil {
				return err
			}
			items = append(items, SourceItem{
				Info: wire.FileInfo{
					Index:        *indexCounter,
					RelativePath: rel,
					Size:         uint64(info.Size()),
					Mode:         uint32(info.Mode().Perm()),
					ModTime:      info.ModTime().UnixNano(),
					Blake3Root:   hash,
					Resumable:    true,
				},
				AbsPath:   path,
				Resumable: true,
			})
			*indexCounter++
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// WalkDir already emits in lexicographic order on POSIX, but
	// belt-and-suspenders sort for cross-platform determinism.
	sort.SliceStable(items[1:], func(i, j int) bool {
		return items[1+i].Info.RelativePath < items[1+j].Info.RelativePath
	})
	// After sort, re-number indexes to stay 0..N-1 (sort may have reordered).
	for i := range items {
		items[i].Info.Index = uint32(i) + (items[0].Info.Index)
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
	defer f.Close()
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
