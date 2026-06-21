package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/polius/fsend/internal/fserrors"
	"github.com/polius/fsend/internal/wire"
)

// Source is one entry to send: its wire listing metadata plus the local
// path to read (empty for stream mode).
type Source struct {
	Entry   wire.ListingEntry
	AbsPath string // sender-local absolute path; "" for synthetic/stream
}

// Walk expands CLI arguments into an ordered Source list — files, directories
// (recursively), and symlinks — using stat only. No file contents are read;
// the BLAKE3 root is computed inline at send time.
//
// RelativePath is rooted at each argument's base name, so `~/proj/foo` lands
// as `foo/...`. Duplicate roots and case-only collisions (the receiver may be
// on a case-insensitive filesystem) are rejected up front.
func Walk(paths []string, excludes []string) ([]Source, error) {
	if len(paths) == 0 {
		return nil, fmt.Errorf("walk: no paths provided")
	}
	m, err := newExcludeMatcher(excludes)
	if err != nil {
		return nil, err
	}

	var out []Source
	seen := make(map[string]string) // lowercased relpath → original (collision check)
	roots := make(map[string]string, len(paths))
	for _, raw := range paths {
		abs, err := filepath.Abs(raw)
		if err != nil {
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		st, err := os.Lstat(abs)
		if err != nil {
			if os.IsNotExist(err) {
				return nil, fmt.Errorf("%w: %s", fserrors.ErrSourceNotFound, raw)
			}
			return nil, fmt.Errorf("walk: %s: %w", raw, err)
		}
		// `.` / `./` / `foo/.` means "send this directory's contents", so its
		// children land at the top level instead of inside a wrapper folder.
		if IsContentsRef(raw) && st.IsDir() {
			children, err := os.ReadDir(abs)
			if err != nil {
				return nil, fmt.Errorf("walk: readdir %s: %w", abs, err)
			}
			sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
			for _, c := range children {
				cAbs := filepath.Join(abs, c.Name())
				cst, err := os.Lstat(cAbs)
				if err != nil {
					return nil, fmt.Errorf("walk: lstat %s: %w", cAbs, err)
				}
				if err := addEntry(&out, seen, cAbs, c.Name(), cst, m); err != nil {
					return nil, err
				}
			}
			continue
		}
		root := filepath.Base(abs)
		if prev, ok := roots[strings.ToLower(root)]; ok {
			return nil, fmt.Errorf("%w: %s and %s would both arrive as %q — rename one before sending", fserrors.ErrUsage, prev, raw, root)
		}
		roots[strings.ToLower(root)] = raw
		if err := addEntry(&out, seen, abs, root, st, m); err != nil {
			return nil, err
		}
	}
	// Assign indices in final order.
	for i := range out {
		out[i].Entry.Index = uint32(i)
	}
	return out, nil
}

// addEntry appends one filesystem entry and recurses into directories. rel is
// the forward-slash path as seen inside the transfer.
func addEntry(out *[]Source, seen map[string]string, abs, rel string, st os.FileInfo, m excludeMatcher) error {
	if m.match(rel) {
		return nil
	}
	if prev, ok := seen[strings.ToLower(rel)]; ok {
		return fmt.Errorf("%w: %s and %s collide on a case-insensitive filesystem — rename one", fserrors.ErrUsage, prev, rel)
	}
	seen[strings.ToLower(rel)] = rel

	mode := st.Mode()
	e := wire.ListingEntry{
		RelativePath: filepath.ToSlash(rel),
		Mode:         uint32(mode.Perm()),
		ModTimeSec:   st.ModTime().Unix(),
	}

	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(abs)
		if err != nil {
			return fmt.Errorf("walk: readlink %s: %w", abs, err)
		}
		e.Type = wire.EntrySymlink
		e.SymlinkTarget = target
		*out = append(*out, Source{Entry: e, AbsPath: abs})
		return nil

	case st.IsDir():
		e.Type = wire.EntryDir
		*out = append(*out, Source{Entry: e, AbsPath: abs})
		children, err := os.ReadDir(abs)
		if err != nil {
			return fmt.Errorf("walk: readdir %s: %w", abs, err)
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name() < children[j].Name() })
		for _, c := range children {
			cAbs := filepath.Join(abs, c.Name())
			cRel := rel + "/" + c.Name()
			cst, err := os.Lstat(cAbs)
			if err != nil {
				return fmt.Errorf("walk: lstat %s: %w", cAbs, err)
			}
			if err := addEntry(out, seen, cAbs, cRel, cst, m); err != nil {
				return err
			}
		}
		return nil

	default:
		// Skip non-regular files (fifo/device/socket): reading one blocks
		// or can't be copied. Mirrors the receiver's handling.
		if !mode.IsRegular() {
			return nil
		}
		e.Type = wire.EntryFile
		e.Size = uint64(st.Size())
		*out = append(*out, Source{Entry: e, AbsPath: abs})
		return nil
	}
}

// IsContentsRef reports whether p explicitly names a directory's *contents*
// rather than the directory itself: ".", "./", or any path ending in "/.".
// A bare trailing slash ("foo/") is intentionally NOT treated this way —
// shells/tab-completion append it freely, and silently spreading the contents
// would be a nasty surprise (rsync's well-known trailing-slash footgun).
func IsContentsRef(p string) bool {
	s := strings.TrimRight(filepath.ToSlash(p), "/")
	return s == "." || strings.HasSuffix(s, "/.")
}

// CountFiles returns the number of regular files + symlinks in sources — the
// user-facing "N files" count (directories aren't counted).
func CountFiles(sources []Source) int {
	n := 0
	for _, s := range sources {
		if s.Entry.Type != wire.EntryDir {
			n++
		}
	}
	return n
}

// TotalBytes sums the sizes across sources.
func TotalBytes(sources []Source) uint64 {
	var t uint64
	for _, s := range sources {
		t += s.Entry.Size
	}
	return t
}
